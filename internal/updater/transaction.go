package updater

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/schema"
)

// This file is the post-download half of `warden update`, modelled as an
// explicit transaction: capture → preflight → swap → restart → readiness, with
// a verified rollback on any failure. Every side effect sits behind a small
// interface so the whole state machine is testable with fakes.

// DefaultReadyTimeout is the overall deadline for the restarted daemon to
// report healthy on the target version. Generous on purpose: a first start
// after an upgrade may replay stores and run migrations.
const DefaultReadyTimeout = 90 * time.Second

// ServiceKind identifies what supervises the daemon.
type ServiceKind string

const (
	ServiceSystemd ServiceKind = "systemd-user"
	ServiceLaunchd ServiceKind = "launchd"
	ServiceManual  ServiceKind = "manual" // a daemon answers but no manager owns it
	ServiceNone    ServiceKind = "none"   // nothing installed, nothing running
)

// ServiceState is the pre-update view of the daemon's supervisor.
type ServiceState struct {
	Kind   ServiceKind
	Active bool   // the managed unit reports running
	Detail string // free-form (unit name, state) for the transcript
}

// ErrNoServiceManager is returned by ServiceController.Restart when no service
// manager owns the daemon; the update then only swaps the binary.
var ErrNoServiceManager = errors.New("no service manager owns the warden daemon")

// ServiceController restarts and inspects the daemon's supervisor.
type ServiceController interface {
	State(ctx context.Context) ServiceState
	Restart(ctx context.Context) error
	Stop(ctx context.Context) error
	// Exited reports whether the supervised process has stopped or is crash
	// looping (best effort; false when unknown).
	Exited(ctx context.Context) bool
	// Diagnostics returns the recent daemon stderr/journal so the real startup
	// error reaches the user. Empty when unavailable.
	Diagnostics(ctx context.Context) string
}

// Health is the decoded GET /healthz payload.
type Health struct {
	Status  string
	Version string
	// SchemaVersion is the data-format version the daemon serves (internal/
	// schema); 0 when the daemon predates the schema ledger and omits it.
	SchemaVersion int
}

// schemaAny disables the schema half of the readiness check (no resolver).
const schemaAny = -1

// Prober queries the daemon's health endpoint.
type Prober interface {
	Probe(ctx context.Context) (Health, error)
}

// BinaryInstaller swaps and restores the installed binary.
type BinaryInstaller interface {
	Swap(newBin string) (backup string, err error)
	Restore(backup string) error
	Discard(backup string)
}

// Clock abstracts time so readiness polling is deterministic in tests.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration)
}

// PreflightResult is what the pre-swap integrity check found.
type PreflightResult struct {
	Blockers      []string // findings that would stop the daemon booting
	Notes         []string // auto-recoverable findings: reported, not blocking
	RepairCommand string
}

// PreState is the captured pre-update state.
type PreState struct {
	BinaryPath    string
	Version       string
	Service       ServiceState
	DaemonRunning bool
	DaemonVersion string
	// DaemonSchema is the schema_version the running daemon advertised (0 when
	// it predates the ledger). Informational, like DaemonVersion: the process
	// may be older than the binary on disk.
	DaemonSchema int
}

// Typed failures. FailureError wraps one of the first five.

// PreflightError: the swap never happened.
type PreflightError struct {
	Blockers      []string
	RepairCommand string
}

func (e *PreflightError) Error() string {
	msg := "update blocked before any change: the backend store cannot be opened by a new daemon:\n  - " +
		strings.Join(e.Blockers, "\n  - ")
	if e.RepairCommand != "" {
		msg += "\nrepair it, then re-run the update:\n  " + e.RepairCommand
	}
	return msg
}

// StartupFailureError: the daemon process exited before becoming healthy.
type StartupFailureError struct{ Diagnostics string }

func (e *StartupFailureError) Error() string {
	return "daemon exited during startup before becoming healthy" + diagSuffix(e.Diagnostics)
}

// ReadinessTimeoutError: the daemon never became healthy before the deadline.
type ReadinessTimeoutError struct {
	Timeout     time.Duration
	LastErr     error
	Diagnostics string
}

func (e *ReadinessTimeoutError) Error() string {
	last := ""
	if e.LastErr != nil {
		last = " (last probe: " + e.LastErr.Error() + ")"
	}
	return fmt.Sprintf("daemon not healthy within %s%s%s", e.Timeout, last, diagSuffix(e.Diagnostics))
}

// WrongVersionError: a healthy daemon advertises a version other than wanted.
type WrongVersionError struct{ Want, Got string }

func (e *WrongVersionError) Error() string {
	got := e.Got
	if got == "" {
		got = "(none)"
	}
	return fmt.Sprintf("daemon is healthy but advertises version %s, expected %s", got, e.Want)
}

// WrongSchemaError: a healthy daemon on the wanted version serves a data
// schema other than the one the target binary writes. 0 means "not reported"
// (a binary that predates the schema ledger).
type WrongSchemaError struct{ Want, Got int }

func (e *WrongSchemaError) Error() string {
	return fmt.Sprintf("daemon is healthy but serves data schema %s, expected %s", schemaLabel(e.Got), schemaLabel(e.Want))
}

func schemaLabel(v int) string {
	if v == 0 {
		return "(none reported)"
	}
	return fmt.Sprint(v)
}

// RollbackError: restoring the previous version did not succeed.
type RollbackError struct {
	Cause       error
	SnapshotDir string
	InstallBin  string
	DataDir     string
}

func (e *RollbackError) Error() string {
	msg := "rollback failed: " + e.Cause.Error()
	if e.SnapshotDir != "" || e.InstallBin != "" {
		snap := e.SnapshotDir
		if snap == "" {
			snap = "<snapshot>"
		}
		data := e.DataDir
		if data == "" {
			data = "<data-dir>"
		}
		bin := e.InstallBin
		if bin == "" {
			bin = "<install-bin>"
		}
		msg += fmt.Sprintf("\nmanual recovery:\n  1. copy data from %s to %s\n  2. restore binary from %s.bak to %s\n  3. restart the warden service",
			snap, data, bin, bin)
	}
	return msg
}
func (e *RollbackError) Unwrap() error { return e.Cause }

// FailureError reports both the original failure and the rollback outcome.
type FailureError struct {
	Cause    error // StartupFailureError, ReadinessTimeoutError, WrongVersionError, or a step error
	Rollback error // nil = rolled back and verified; *RollbackError otherwise
	Prior    string
}

func (e *FailureError) Unwrap() []error { return []error{e.Cause, e.Rollback} }

func (e *FailureError) Error() string {
	var b strings.Builder
	b.WriteString("update failed: " + headline(e.Cause) + "\n" + e.Cause.Error())
	if e.Rollback == nil {
		fmt.Fprintf(&b, "\nrollback: restored v%s, restarted the service, and verified it healthy", e.Prior)
	} else {
		fmt.Fprintf(&b, "\nROLLBACK FAILED — the warden daemon may be down: %v\nrestore manually from <install-bin>.bak and restart the service", e.Rollback)
	}
	return b.String()
}

func headline(err error) string {
	var (
		su *StartupFailureError
		rt *ReadinessTimeoutError
		wv *WrongVersionError
		ws *WrongSchemaError
	)
	switch {
	case errors.As(err, &ws):
		return "wrong data schema"
	case errors.As(err, &su):
		return "startup failure"
	case errors.As(err, &rt):
		return "readiness timeout"
	case errors.As(err, &wv):
		return "wrong version"
	}
	return "step failed"
}

func diagSuffix(d string) string {
	d = strings.TrimSpace(d)
	if d == "" {
		return "\n(no daemon diagnostics available — check the service logs)"
	}
	return "\ndaemon diagnostics:\n" + indent(d)
}

func indent(s string) string {
	return "  | " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  | ")
}

// txn carries the collaborators for one run.
type txn struct {
	opts        Options
	svc         ServiceController
	probe       Prober
	inst        BinaryInstaller
	clock       Clock
	pre         PreState
	target      string
	snapshotDir string
	stopped     bool
}

func (t *txn) logf(format string, a ...any) { fmt.Fprintf(t.opts.Stdout, format+"\n", a...) }

func (t *txn) capture(ctx context.Context) {
	t.pre = PreState{BinaryPath: t.opts.InstallBin, Version: stripV(t.opts.CurrentVersion), Service: t.svc.State(ctx)}
	if h, err := t.probe.Probe(ctx); err == nil && h.Status == "ok" {
		t.pre.DaemonRunning, t.pre.DaemonVersion, t.pre.DaemonSchema = true, stripV(h.Version), h.SchemaVersion
	}
	running := "not running"
	if t.pre.DaemonRunning {
		running = "running v" + t.pre.DaemonVersion
	}
	t.logf("pre-update state: binary %s v%s; service %s; daemon %s",
		t.pre.BinaryPath, t.pre.Version, t.pre.Service.Kind, running)
}

func (t *txn) preflight(ctx context.Context) error {
	if t.opts.Preflight == nil {
		return nil
	}
	res, err := t.opts.Preflight(ctx)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}
	for _, n := range res.Notes {
		t.logf("preflight: auto-recoverable (not blocking): %s", n)
	}
	if len(res.Blockers) > 0 {
		return &PreflightError{Blockers: res.Blockers, RepairCommand: res.RepairCommand}
	}
	t.logf("preflight: backend store ok")
	return nil
}

func (t *txn) targetPreflight(ctx context.Context, targetBin string) error {
	if t.opts.TargetPreflight == nil {
		return nil
	}
	res, err := t.opts.TargetPreflight(ctx, targetBin)
	if err != nil {
		return fmt.Errorf("target preflight: %w", err)
	}
	for _, n := range res.Notes {
		t.logf("target preflight: auto-recoverable (not blocking): %s", n)
	}
	if len(res.Blockers) > 0 {
		return &PreflightError{Blockers: res.Blockers, RepairCommand: res.RepairCommand}
	}
	t.logf("target preflight: all stores ok")
	return nil
}

// waitReady polls with bounded backoff until health is ok on wantVersion
// ("" = any version) AND wantSchema (schemaAny = any), the process exits, or
// the deadline passes. Both must match: a daemon on the right binary serving
// the wrong data format is not a finished update.
func (t *txn) waitReady(ctx context.Context, wantVersion string, wantSchema int) error {
	deadline := t.clock.Now().Add(t.opts.ReadyTimeout)
	delay := 250 * time.Millisecond
	var lastErr error
	var gotVersion string
	var gotSchema int
	versionMismatch, schemaMismatch := false, false
	for {
		if t.svc.Exited(ctx) {
			return &StartupFailureError{Diagnostics: t.svc.Diagnostics(ctx)}
		}
		h, err := t.probe.Probe(ctx)
		switch {
		case err != nil:
			lastErr, versionMismatch, schemaMismatch = err, false, false
		case h.Status != "ok":
			lastErr, versionMismatch, schemaMismatch = fmt.Errorf("health status %q", h.Status), false, false
		case wantVersion != "" && stripV(h.Version) != wantVersion:
			versionMismatch, schemaMismatch, gotVersion = true, false, stripV(h.Version)
		case wantSchema != schemaAny && h.SchemaVersion != wantSchema:
			versionMismatch, schemaMismatch, gotSchema = false, true, h.SchemaVersion
		default:
			return nil
		}
		if !t.clock.Now().Before(deadline) || ctx.Err() != nil {
			if versionMismatch {
				return &WrongVersionError{Want: wantVersion, Got: gotVersion}
			}
			if schemaMismatch {
				return &WrongSchemaError{Want: wantSchema, Got: gotSchema}
			}
			return &ReadinessTimeoutError{Timeout: t.opts.ReadyTimeout, LastErr: lastErr, Diagnostics: t.svc.Diagnostics(ctx)}
		}
		t.clock.Sleep(ctx, delay)
		if delay = delay * 3 / 2; delay > 2*time.Second {
			delay = 2 * time.Second
		}
	}
}

// rollback restores the previous binary and data snapshot, explicitly restarts the service when
// the new daemon had been started or was previously running, and verifies the prior version is healthy.
func (t *txn) rollback(ctx context.Context, backup, snapDir string, restarted bool) error {
	t.logf("rolling back to v%s…", t.pre.Version)
	var rbErrs []error
	if err := t.inst.Restore(backup); err != nil {
		rbErrs = append(rbErrs, fmt.Errorf("restore binary: %w", err))
	}
	if snapDir != "" && t.opts.DataDir != "" {
		if err := RestoreSnapshot(snapDir, t.opts.DataDir); err != nil {
			rbErrs = append(rbErrs, fmt.Errorf("restore data snapshot: %w", err))
		}
	}
	if len(rbErrs) > 0 {
		return &RollbackError{
			Cause:       errors.Join(rbErrs...),
			SnapshotDir: snapDir,
			InstallBin:  t.opts.InstallBin,
			DataDir:     t.opts.DataDir,
		}
	}
	if restarted || t.stopped {
		if err := t.svc.Restart(ctx); err != nil && !errors.Is(err, ErrNoServiceManager) {
			return &RollbackError{
				Cause:       fmt.Errorf("restart previous service: %w", err),
				SnapshotDir: snapDir,
				InstallBin:  t.opts.InstallBin,
				DataDir:     t.opts.DataDir,
			}
		}
	}
	if !t.pre.DaemonRunning || (!restarted && !t.stopped) {
		return nil // nothing was serving before or daemon was never touched; nothing to verify
	}
	want := t.pre.Version
	if want == "" || want == "dev" {
		want = ""
	}
	// The restored binary is the one running this updater, so it must come
	// back serving the schema that binary writes (nothing here migrates data).
	wantSchema := schemaAny
	if t.opts.TargetSchema != nil {
		wantSchema = t.opts.CurrentSchema
	}
	if err := t.waitReady(ctx, want, wantSchema); err != nil {
		return &RollbackError{
			Cause:       fmt.Errorf("previous version not healthy after restore: %w", err),
			SnapshotDir: snapDir,
			InstallBin:  t.opts.InstallBin,
			DataDir:     t.opts.DataDir,
		}
	}
	return nil
}

// apply runs stop → snapshot → swap → codesign → migrate → restart → readiness on a staged binary.
func (t *txn) apply(ctx context.Context, staged string) (rolledBack bool, err error) {
	if t.opts.DataDir != "" && t.pre.DaemonRunning {
		t.logf("stopping daemon…")
		if err := t.svc.Stop(ctx); err != nil && !errors.Is(err, ErrNoServiceManager) {
			return false, fmt.Errorf("stop daemon: %w", err)
		}
		t.stopped = true
	}

	var snapDir string
	if t.opts.DataDir != "" {
		t.logf("taking data snapshot…")
		var err error
		snapDir, err = SnapshotStores(t.opts.DataDir, t.pre.Version, t.opts.InstallBin)
		if err != nil {
			if t.stopped {
				_ = t.svc.Restart(ctx)
			}
			return false, fmt.Errorf("snapshot data: %w", err)
		}
		t.snapshotDir = snapDir

		// Record snapshot in ledger journal
		if l, err := schema.Load(t.opts.DataDir); err == nil {
			l.InProgress = &schema.InProgress{
				Step:     "snapshot",
				Snapshot: snapDir,
			}
			_ = schema.Save(t.opts.DataDir, l)
		}
	}

	backup, err := t.inst.Swap(staged)
	if err != nil {
		if snapDir != "" && t.opts.DataDir != "" {
			_ = RestoreSnapshot(snapDir, t.opts.DataDir)
		}
		if t.pre.DaemonRunning {
			_ = t.svc.Restart(ctx)
		}
		return false, err
	}
	restarted := false
	fail := func(cause error) (bool, error) {
		rb := t.rollback(ctx, backup, snapDir, restarted)
		return rb == nil, &FailureError{Cause: cause, Rollback: rb, Prior: t.pre.Version}
	}
	if t.opts.Codesign != nil {
		if err := t.opts.Codesign(t.opts.InstallBin); err != nil {
			return fail(fmt.Errorf("codesign: %w", err))
		}
	}
	if t.opts.Migrate != nil {
		t.logf("running migrations…")
		if err := t.opts.Migrate(); err != nil {
			return fail(fmt.Errorf("migrate: %w", err))
		}
	}
	wantSchema := schemaAny
	if t.opts.TargetSchema != nil {
		v, err := t.opts.TargetSchema(ctx, t.opts.InstallBin)
		if err != nil {
			return fail(fmt.Errorf("read the new binary's data schema version: %w", err))
		}
		wantSchema = v
	}
	t.logf("restarting service…")
	restarted = true
	if err := t.svc.Restart(ctx); err != nil {
		if errors.Is(err, ErrNoServiceManager) {
			t.inst.Discard(backup)
			t.logf("no service manager owns the daemon: the binary is updated; restart the warden daemon manually to run v%s", t.target)
			return false, nil
		}
		diag := err.Error()
		if d := strings.TrimSpace(t.svc.Diagnostics(ctx)); d != "" {
			diag += "\n" + d
		}
		return fail(&StartupFailureError{Diagnostics: diag})
	}
	if wantSchema == schemaAny {
		t.logf("waiting up to %s for the daemon to report v%s…", t.opts.ReadyTimeout, t.target)
	} else {
		t.logf("waiting up to %s for the daemon to report v%s on data schema %d…", t.opts.ReadyTimeout, t.target, wantSchema)
	}
	if err := t.waitReady(ctx, t.target, wantSchema); err != nil {
		return fail(err)
	}

	if t.opts.DataDir != "" {
		if l, err := schema.Load(t.opts.DataDir); err == nil && l.InProgress != nil {
			l.InProgress = nil
			_ = schema.Save(t.opts.DataDir, l)
		}
		pruned, _ := PruneSnapshots(t.opts.DataDir, t.clock, 2, 14*24*time.Hour)
		if len(pruned) > 0 {
			t.logf("pruned %d old snapshot(s)", len(pruned))
		}
	}

	t.inst.Discard(backup)
	return false, nil
}
