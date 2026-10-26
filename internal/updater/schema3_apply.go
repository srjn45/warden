package updater

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// TargetInfo is what a freshly staged binary reports about itself
// (`warden version --json`). The updater binds it to the release manifest
// before any source mutation.
type TargetInfo struct {
	Version      string
	Schema       int
	Capabilities map[string]int
}

// ReadinessEvidence identifies the exact transaction a started target daemon
// must prove ownership of.
type ReadinessEvidence struct {
	Transaction      string
	DataDir          string
	TargetBinaryPath string
	TargetBinarySHA  string
	SourceFormat     int
	TargetFormat     int
	ManifestSHA256   string
	TargetVersion    string
	ObservedHealth   Health
	TransactionLedge string
}

// Schema3Hooks are the production seams of the format-3 update path. Every
// field is optional only in the sense that a nil hook fails the schema-3 path
// closed; a schema-2 update never consults them.
type Schema3Hooks struct {
	// ConfigPaths lists the top-level data-dir files the update may alter and a
	// rollback must restore (names relative to the data dir).
	ConfigPaths func() ([]string, error)
	// ReconcileConfig performs the config migration after the paired backup.
	ReconcileConfig func() error
	// TargetInfo inspects the staged binary.
	TargetInfo func(ctx context.Context, bin string) (TargetInfo, error)
	// FreeBytes measures space available to the data dir (default: statfs).
	FreeBytes func(dir string) (uint64, error)
	// VerifyReadiness asks the started target to prove exact version, format
	// and ownership for the transaction. false/err fails the update closed.
	VerifyReadiness func(ctx context.Context, ev ReadinessEvidence) (bool, error)
}

func newSchema3TransactionID(now time.Time) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

// schema3TargetGate is the capability-gated lift of the old "format 3 refused"
// guard: a format-3 target is accepted only when a validated manifest, the
// running updater's exact capability set, the staged binary's own report and the
// installed schema-2 data all agree.
func (t *txn) schema3TargetGate(ctx context.Context, targetBin string) error {
	h := t.opts.Schema3
	if h == nil || h.TargetInfo == nil || h.VerifyReadiness == nil || h.ConfigPaths == nil || h.ReconcileConfig == nil {
		return fmt.Errorf("schema3: updater hooks are not configured; refusing format-3 target")
	}
	m := t.manifest
	if m == nil {
		return fmt.Errorf("schema3: release manifest required for a format-3 target")
	}
	if err := ValidateSchema3Release(*m); err != nil {
		return err
	}
	if stripV(m.Version) != stripV(t.target) {
		return fmt.Errorf("schema3: manifest version %s does not match release %s", m.Version, t.target)
	}
	cur, err := ParseSemver(t.pre.Version)
	if err != nil {
		return fmt.Errorf("schema3: running updater version %q is not releasable: %w", t.pre.Version, err)
	}
	min, err := ParseSemver(m.MinUpgradeFrom)
	if err != nil {
		return err
	}
	if cur.Compare(min) < 0 {
		return fmt.Errorf("schema3: running updater v%s predates the required waypoint v%s; update to the waypoint first", cur, min)
	}
	if t.opts.CurrentSchema != 2 {
		return fmt.Errorf("schema3: committed schema 2 required, running binary writes schema %d", t.opts.CurrentSchema)
	}
	info, err := h.TargetInfo(ctx, targetBin)
	if err != nil {
		return fmt.Errorf("schema3: inspect target binary: %w", err)
	}
	if stripV(info.Version) != stripV(m.Version) || info.Schema != 3 {
		return fmt.Errorf("schema3: target binary reports v%s/schema %d, manifest says v%s/schema 3", info.Version, info.Schema, m.Version)
	}
	want := Schema3Requirements()
	if len(info.Capabilities) != len(want) {
		return fmt.Errorf("schema3: target binary capabilities %v do not match the manifest requirements", info.Capabilities)
	}
	for name, v := range want {
		if info.Capabilities[name] != v || m.Requires.Capabilities[name] != v {
			return fmt.Errorf("schema3: capability %s not bound by both binary and manifest", name)
		}
	}
	return nil
}

func defaultFreeBytes(dir string) (uint64, error) { return diskFree(dir) }

// applySchema3 is the format-3 branch of apply: stop → exclusive ownership →
// paired backup → swap → config → promote → handoff → restart → readiness →
// finalize. Any failure after the backup is sealed restores the complete paired
// old binary and data generation and proves the old daemon healthy.
func (t *txn) applySchema3(ctx context.Context, staged string) (rolledBack bool, err error) {
	h := t.opts.Schema3
	if t.opts.DataDir == "" {
		return false, fmt.Errorf("schema3: a data directory is required")
	}
	if t.opts.Codesign != nil {
		// Sign before hashing so the paired identity is the installed bytes.
		if err := t.opts.Codesign(staged); err != nil {
			return false, fmt.Errorf("codesign: %w", err)
		}
	}
	id, err := newSchema3TransactionID(time.Now())
	if err != nil {
		return false, err
	}
	tr, err := schema3Path(t.opts.DataDir, id)
	if err != nil {
		return false, err
	}
	bin, err := filepath.Abs(t.opts.InstallBin)
	if err != nil {
		return false, err
	}
	wasRunning := t.pre.DaemonRunning
	t.logf("schema-3 update transaction %s: stopping daemon…", id)
	if err = acquireSchema3(ctx, tr, t.svc, t.probe); err != nil {
		if wasRunning {
			_ = t.svc.Restart(ctx)
		}
		return false, err
	}
	abort := func(cause error) (bool, error) {
		if tr.lock != nil {
			_ = tr.close()
		}
		if wasRunning {
			_ = t.svc.Restart(ctx)
		}
		return false, cause
	}
	paths, err := h.ConfigPaths()
	if err != nil {
		return abort(err)
	}
	free := h.FreeBytes
	if free == nil {
		free = defaultFreeBytes
	}
	avail, err := free(tr.dir)
	if err != nil {
		return abort(fmt.Errorf("schema3: measure free space: %w", err))
	}
	t.logf("taking verified paired backup (old binary + data)…")
	if err = tr.begin(ctx, bin, staged, t.pre.Version, t.target, paths, avail); err != nil {
		return abort(err)
	}
	t.logf("rollback after this point: warden rollback --transaction %s --accept-data-loss", id)

	backup := ""
	swapped := false
	restarted := false
	fail := func(cause error) (bool, error) {
		rb := t.rollbackSchema3(ctx, tr, bin, backup, swapped, restarted)
		return rb == nil, &FailureError{Cause: cause, Rollback: rb, Prior: t.pre.Version}
	}
	if backup, err = t.inst.Swap(staged); err != nil {
		return fail(fmt.Errorf("swap binary: %w", err))
	}
	swapped = true
	t.logf("reconciling config…")
	if err = h.ReconcileConfig(); err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}
	t.logf("migrating data to schema 3…")
	if err = tr.promote(ctx); err != nil {
		return fail(fmt.Errorf("migrate: %w", err))
	}
	if err = tr.handoff(); err != nil {
		return fail(err)
	}
	t.logf("restarting service on schema 3…")
	restarted = true
	if err = t.svc.Restart(ctx); err != nil {
		diag := err.Error()
		if d := t.svc.Diagnostics(ctx); d != "" {
			diag += "\n" + d
		}
		return fail(&StartupFailureError{Diagnostics: diag})
	}
	if err = t.waitReady(ctx, t.target, 3); err != nil {
		return fail(err)
	}
	health, err := t.probe.Probe(ctx)
	if err != nil {
		return fail(err)
	}
	ok, err := h.VerifyReadiness(ctx, ReadinessEvidence{
		Transaction: id, DataDir: tr.dir, TargetBinaryPath: bin, TargetBinarySHA: tr.manifest.TargetBinaryHash, SourceFormat: 2, TargetFormat: 3,
		ManifestSHA256: tr.manifestHash, TargetVersion: t.target, ObservedHealth: health,
	})
	if err != nil || !ok {
		if err == nil {
			err = errors.New("target did not prove ownership")
		}
		return fail(fmt.Errorf("schema3: readiness: %w", err))
	}
	if err = tr.finish(ctx, health, true); err != nil {
		return fail(err)
	}
	t.inst.Discard(backup)
	if pruned, _ := PruneSnapshots(t.opts.DataDir, t.clock, 2, 14*24*time.Hour); len(pruned) > 0 {
		t.logf("pruned %d old snapshot(s)", len(pruned))
	}
	return false, nil
}

// rollbackSchema3 restores the sealed paired generation after a failed
// format-3 update and proves the exact old daemon healthy again.
func (t *txn) rollbackSchema3(ctx context.Context, tr *schema3Transaction, bin, backup string, swapped, restarted bool) error {
	t.logf("rolling back to the paired old binary and data…")
	if restarted || tr.lock == nil {
		// A target daemon may be running or crash-looping: stop it and take the
		// owner lock back before any restoration.
		_ = t.svc.Stop(ctx)
		if err := acquireSchema3(ctx, tr, t.svc, t.probe); err != nil {
			return fmt.Errorf("schema3 rollback: %w", err)
		}
	}
	if err := tr.restore(ctx, bin); err != nil {
		_ = tr.close()
		return fmt.Errorf("schema3 rollback: %w", err)
	}
	if swapped && backup != "" {
		t.inst.Discard(backup)
	}
	if err := restartPairedOld(ctx, tr, t.svc, t.probe, t.clock, t.opts.ReadyTimeout); err != nil {
		return err
	}
	return nil
}
