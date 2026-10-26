package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/schema"
	"github.com/stretchr/testify/require"
)

type schema3ApplyHarness struct {
	t         *testing.T
	dir       string
	bin       string
	staged    string
	svc       *pairedRestoreService
	out       *bytes.Buffer
	manifest  Manifest
	info      TargetInfo
	targetOK  bool // the started target reports healthy format 3
	verify    func(ReadinessEvidence) (bool, error)
	reconcile func() error
}

func newSchema3ApplyHarness(t *testing.T) *schema3ApplyHarness {
	tr, old, target := schema3Fixture(t)
	require.NoError(t, tr.close()) // the updater, not the fixture, owns the lock here
	h := &schema3ApplyHarness{t: t, dir: tr.dir, bin: old, staged: target, svc: &pairedRestoreService{}, out: &bytes.Buffer{}, targetOK: true}
	h.manifest = Manifest{Version: "13.0.0", SchemaVersion: 3, MinSchema: 2, Breaking: true, MinUpgradeFrom: "12.1.0",
		Requires: ManifestRequirements{Capabilities: Schema3Requirements()}, Notes: []string{"ownership normalization and paired-data rollback"}}
	h.info = TargetInfo{Version: "13.0.0", Schema: 3, Capabilities: Schema3Requirements()}
	h.verify = func(ReadinessEvidence) (bool, error) { return true, nil }
	h.reconcile = func() error {
		return os.WriteFile(filepath.Join(h.dir, "config.yaml"), []byte("setting: migrated\n"), 0o600)
	}
	return h
}

func (h *schema3ApplyHarness) txn() *txn {
	probe := &fakeProbe{fn: func() (Health, error) {
		if h.svc.stopped {
			return Health{}, errors.New("stopped")
		}
		// The running daemon is whatever binary is installed.
		if raw, _ := os.ReadFile(h.bin); string(raw) == "target binary" {
			if h.targetOK {
				return Health{Status: "ok", Version: "13.0.0", SchemaVersion: 3}, nil
			}
			return Health{Status: "ok", Version: "13.0.0", SchemaVersion: 2}, nil
		}
		return Health{Status: "ok", Version: "12.1.0", SchemaVersion: 2}, nil
	}}
	hooks := &Schema3Hooks{
		ConfigPaths:     func() ([]string, error) { return []string{"config.yaml"}, nil },
		ReconcileConfig: func() error { return h.reconcile() },
		TargetInfo:      func(context.Context, string) (TargetInfo, error) { return h.info, nil },
		FreeBytes:       func(string) (uint64, error) { return 1 << 50, nil },
		VerifyReadiness: func(_ context.Context, ev ReadinessEvidence) (bool, error) { return h.verify(ev) },
	}
	m := h.manifest
	return &txn{
		opts: Options{CurrentVersion: "12.1.0", InstallBin: h.bin, DataDir: h.dir, Stdout: h.out, ReadyTimeout: 5 * time.Second,
			TargetSchema: func(context.Context, string) (int, error) { return 3, nil }, CurrentSchema: 2, Schema3: hooks},
		svc: h.svc, probe: probe, inst: fsInstaller{bin: h.bin}, clock: newFakeClock(), target: "13.0.0", manifest: &m,
	}
}

func TestSchema3ApplyHappyPathFinalizesLedger(t *testing.T) {
	h := newSchema3ApplyHarness(t)
	tx := h.txn()
	ctx := context.Background()
	tx.capture(ctx)
	require.NoError(t, tx.targetPreflight(ctx, h.staged))
	rolledBack, err := tx.apply(ctx, h.staged)
	require.NoError(t, err, h.out.String())
	require.False(t, rolledBack)
	l, err := schema.Load(h.dir)
	require.NoError(t, err)
	require.Equal(t, 3, l.SchemaVersion)
	require.Nil(t, l.InProgress, "readiness must be finalized")
	require.Equal(t, 1, h.svc.starts, "target started exactly once")
	got, _ := os.ReadFile(h.bin)
	require.Equal(t, "target binary", string(got))
	require.Contains(t, h.out.String(), "warden rollback --transaction")
}

func TestSchema3ApplyFailuresRestorePairedOldGeneration(t *testing.T) {
	for name, mutate := range map[string]func(*schema3ApplyHarness){
		"target never reports format 3": func(h *schema3ApplyHarness) { h.targetOK = false },
		"ownership not proven":          func(h *schema3ApplyHarness) { h.verify = func(ReadinessEvidence) (bool, error) { return false, nil } },
		"config reconcile fails":        func(h *schema3ApplyHarness) { h.reconcile = func() error { return errors.New("bad config") } },
		"target service fails to start": func(h *schema3ApplyHarness) {
			h.svc.onStart = func() error {
				if h.svc.starts == 1 {
					return errors.New("unit failed")
				}
				return nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSchema3ApplyHarness(t)
			mutate(h)
			tx := h.txn()
			ctx := context.Background()
			tx.capture(ctx)
			rolledBack, err := tx.apply(ctx, h.staged)
			require.Error(t, err)
			require.True(t, rolledBack, "paired restore must succeed: %v", err)
			var fe *FailureError
			require.ErrorAs(t, err, &fe)
			require.NoError(t, fe.Rollback)
			l, lerr := schema.Load(h.dir)
			require.NoError(t, lerr)
			require.Equal(t, 2, l.SchemaVersion)
			require.Nil(t, l.InProgress)
			got, _ := os.ReadFile(h.bin)
			require.Equal(t, "old binary", string(got))
			cfg, _ := os.ReadFile(filepath.Join(h.dir, "config.yaml"))
			require.Equal(t, "setting: original\n", string(cfg), "config restored")
			require.False(t, h.svc.stopped, "old daemon running again")
		})
	}
}

func TestSchema3TargetGateFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*schema3ApplyHarness, *txn){
		"no manifest":              func(_ *schema3ApplyHarness, tx *txn) { tx.manifest = nil },
		"no hooks":                 func(_ *schema3ApplyHarness, tx *txn) { tx.opts.Schema3 = nil },
		"binary capability absent": func(h *schema3ApplyHarness, _ *txn) { h.info.Capabilities = map[string]int{} },
		"binary capability future": func(h *schema3ApplyHarness, _ *txn) { h.info.Capabilities[CapabilityPairedBackup] = 2 },
		"binary version mismatch":  func(h *schema3ApplyHarness, _ *txn) { h.info.Version = "13.0.1" },
		"binary not format 3":      func(h *schema3ApplyHarness, _ *txn) { h.info.Schema = 2 },
		"manifest capability gap": func(_ *schema3ApplyHarness, tx *txn) {
			tx.manifest.Requires.Capabilities = map[string]int{CapabilityStoppedDaemon: 1}
		},
		"updater predates waypoint": func(_ *schema3ApplyHarness, tx *txn) { tx.pre.Version = "12.0.0" },
		"dev updater":               func(_ *schema3ApplyHarness, tx *txn) { tx.pre.Version = "dev" },
		"source not schema 2":       func(_ *schema3ApplyHarness, tx *txn) { tx.opts.CurrentSchema = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			h := newSchema3ApplyHarness(t)
			tx := h.txn()
			tx.capture(context.Background())
			mutate(h, tx)
			if name == "binary capability future" || name == "binary capability absent" || name == "binary version mismatch" || name == "binary not format 3" {
				tx = h.txn() // hooks read h.info lazily; rebuild with the manifest intact
				tx.capture(context.Background())
			}
			require.Error(t, tx.schema3TargetGate(context.Background(), h.staged))
			// A refused gate never touched the service or the source.
			require.Equal(t, 0, h.svc.starts)
			l, err := schema.Load(h.dir)
			require.NoError(t, err)
			require.Equal(t, 2, l.SchemaVersion)
			require.Nil(t, l.InProgress)
		})
	}
}

func TestSchema3InsufficientSpaceLeavesOldDaemonRunning(t *testing.T) {
	h := newSchema3ApplyHarness(t)
	tx := h.txn()
	tx.opts.Schema3.FreeBytes = func(string) (uint64, error) { return 1, nil }
	ctx := context.Background()
	tx.capture(ctx)
	_, err := tx.apply(ctx, h.staged)
	require.ErrorContains(t, err, "insufficient space")
	require.False(t, h.svc.stopped, "daemon restarted after refused update")
	got, _ := os.ReadFile(h.bin)
	require.Equal(t, "old binary", string(got))
}

// A waypoint release must still update schema-2 installs exactly as before even
// when the format-3 hooks and a (non-format-3) manifest are present.
func TestSchema2UpdateUnaffectedBySchema3Wiring(t *testing.T) {
	f := newSchema2Fixture(t)
	tx := f.txn(func(restarts int) Health {
		if restarts == 0 {
			return Health{Status: "ok", Version: "10.0.1", SchemaVersion: 1}
		}
		return Health{Status: "ok", Version: "11.0.0", SchemaVersion: 2}
	}, f.cutover(t))
	tx.opts.Schema3 = &Schema3Hooks{} // present but unusable: any use would fail closed
	tx.manifest = &Manifest{Version: "11.0.0", SchemaVersion: 2}
	ctx := context.Background()
	tx.capture(ctx)
	require.NoError(t, tx.targetPreflight(ctx, f.stagedPath))
	rolledBack, err := tx.apply(ctx, f.stagedPath)
	require.NoError(t, err, f.out.String())
	require.False(t, rolledBack)
	l, err := schema.Load(f.dataDir)
	require.NoError(t, err)
	require.Equal(t, 2, l.SchemaVersion)
	require.NotContains(t, f.out.String(), "schema-3")
}
