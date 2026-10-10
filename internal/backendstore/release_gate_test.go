package backendstore

// Release-gate regression matrix for issue #841 (t6). Each scenario drives the
// real updater.Apply transaction (archive download + checksum, swap, migrate,
// restart, readiness, rollback) against a data dir holding a real ScrivaDB
// backend registry, with a fake service whose "daemon" opens the registry the
// way the real one does (backendstore.Open). The matrix asserts, per scenario:
// the user's preferences survive, backups restore independently, and no failure
// is reported as a bare "connection refused".

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/updater"
)

type gateClock struct{ now time.Time }

func (c *gateClock) Now() time.Time                           { return c.now }
func (c *gateClock) Sleep(_ context.Context, d time.Duration) { c.now = c.now.Add(d) }

// gateDaemon is both ServiceController and Prober. A restart "starts" the
// daemon that the installed binary represents: the old binary ("old") is a
// healthy v1; the new binary opens the registry via Open and either serves v2
// (after readyAfter probes) or exits with the open error.
type gateDaemon struct {
	t          *testing.T
	bin, dir   string
	readyAfter int // probes that fail with "connection refused" before healthy
	openOpts   Options

	mu       sync.Mutex
	store    *Store
	res      *Result
	running  string // "", "1.0.0", "2.0.0"
	exited   bool
	diag     string
	probes   int
	restarts int
}

func (d *gateDaemon) State(context.Context) updater.ServiceState {
	return updater.ServiceState{Kind: updater.ServiceSystemd, Active: true}
}

func (d *gateDaemon) Restart(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restarts++
	if d.store != nil {
		_ = d.store.Close()
		d.store = nil
	}
	d.probes, d.exited, d.diag, d.running = 0, false, "", ""
	b, err := os.ReadFile(d.bin)
	require.NoError(d.t, err)
	if string(b) == "old" {
		d.running = "1.0.0"
		return nil
	}
	s, res, err := Open(d.dir, d.openOpts)
	if err != nil {
		d.exited, d.diag = true, "warden daemon: "+err.Error()
		return nil
	}
	d.store, d.res, d.running = s, res, "2.0.0"
	return nil
}

func (d *gateDaemon) Stop(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store != nil {
		_ = d.store.Close()
		d.store = nil
	}
	d.running = ""
	return nil
}

func (d *gateDaemon) Exited(context.Context) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exited
}

func (d *gateDaemon) Diagnostics(context.Context) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.diag
}

func (d *gateDaemon) Probe(context.Context) (updater.Health, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.probes++
	if d.running == "" || (d.running == "2.0.0" && d.probes <= d.readyAfter) {
		return updater.Health{}, errors.New("dial tcp 127.0.0.1:8765: connect: connection refused")
	}
	return updater.Health{Status: "ok", Version: d.running}, nil
}

func (d *gateDaemon) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store != nil {
		_ = d.store.Close()
		d.store = nil
	}
}

// gatePreflight mirrors cli.backendPreflight (covered by cli/update_test.go).
func gatePreflight(dir string) func(context.Context) (updater.PreflightResult, error) {
	return func(ctx context.Context) (updater.PreflightResult, error) {
		res := updater.PreflightResult{RepairCommand: RepairCommand}
		if _, err := os.Stat(dir); err != nil {
			return res, nil
		}
		rep, err := Verify(ctx, dir)
		if err != nil {
			res.Blockers = append(res.Blockers, fmt.Sprintf("%s: verification failed: %v", dir, err))
			return res, nil
		}
		for _, c := range rep.Collections {
			switch c.Verdict {
			case VerdictRecoverable:
				res.Notes = append(res.Notes, fmt.Sprintf("%s: %s", c.Name, strings.Join(c.Codes, ", ")))
			case VerdictAmbiguous:
				res.Blockers = append(res.Blockers, fmt.Sprintf("%s: %s (%s)", c.Name, strings.Join(c.Codes, ", "), strings.Join(c.Reasons, "; ")))
			}
		}
		return res, nil
	}
}

type gateEnv struct {
	t       *testing.T
	home    string
	dataDir string // <home>/data; the registry is <dataDir>/backends
	reg     string
	bin     string
	out     *bytes.Buffer
	d       *gateDaemon
	opts    updater.Options
	backups string
}

func newGateEnv(t *testing.T, reg string) *gateEnv {
	t.Helper()
	home := t.TempDir()
	e := &gateEnv{t: t, home: home, dataDir: filepath.Dir(reg), reg: reg, out: &bytes.Buffer{}, backups: t.TempDir()}
	e.bin = filepath.Join(home, "bin", "warden")
	require.NoError(t, os.MkdirAll(filepath.Dir(e.bin), 0o755))
	require.NoError(t, os.WriteFile(e.bin, []byte("old"), 0o755))

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	tw := tar.NewWriter(zw)
	payload := []byte("new")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "warden", Mode: 0o755, Size: int64(len(payload))}))
	_, _ = tw.Write(payload)
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	sum := sha256.Sum256(gz.Bytes())
	sums := hex.EncodeToString(sum[:]) + "  warden_2.0.0_linux_amd64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "warden_2.0.0_linux_amd64.tar.gz":
			_, _ = w.Write(gz.Bytes())
		case "checksums.txt":
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	oo := quiet()
	oo.BackupDir = e.backups
	e.d = &gateDaemon{t: t, bin: e.bin, dir: reg, openOpts: oo, running: "1.0.0"}
	t.Cleanup(e.d.stop)
	e.opts = updater.Options{
		CurrentVersion: "1.0.0", TargetVersion: "2.0.0", GOOS: "linux", GOARCH: "amd64",
		InstallBin: e.bin, StagingDir: filepath.Join(home, "tmp"), DataDir: e.dataDir, AssetBase: srv.URL,
		HTTPClient: srv.Client(), Stdout: e.out, Service: e.d, Prober: e.d,
		Clock: &gateClock{now: time.Unix(1_700_000_000, 0)}, ReadyTimeout: 30 * time.Second,
		Preflight: gatePreflight(reg),
	}
	return e
}

func (e *gateEnv) installed() string { b, _ := os.ReadFile(e.bin); return string(b) }

// reopenSnapshot opens the registry strictly (what the next daemon does) and
// returns the user-owned state.
func (e *gateEnv) reopenSnapshot() registrySnapshot {
	e.t.Helper()
	e.d.stop()
	s, err := NewStore(e.reg)
	require.NoError(e.t, err, "registry must open strictly after the scenario")
	defer s.Close()
	return snapshotRegistry(e.t, s)
}

func (e *gateEnv) requireNoBareConnRefused(err error) {
	e.t.Helper()
	require.Error(e.t, err)
	msg := err.Error()
	if strings.Contains(msg, "connection refused") {
		require.NotEqual(e.t, "connection refused", strings.TrimSpace(msg))
		// the real cause must accompany any probe error
		require.True(e.t, strings.Contains(msg, "warden daemon:") || strings.Contains(msg, "readiness timeout") || strings.Contains(msg, "startup failure"), msg)
	}
}

// corruptIndexes garbles the persisted index files of cols (derived data).
func corruptIndexes(t *testing.T, dir string, cols ...string) {
	t.Helper()
	for _, c := range cols {
		matches, err := filepath.Glob(filepath.Join(dir, c, "*idx*.json"))
		require.NoError(t, err)
		idx, _ := filepath.Glob(filepath.Join(dir, c, "index.json"))
		for _, p := range append(matches, idx...) {
			require.NoError(t, os.WriteFile(p, []byte("{not json"), 0o600))
		}
	}
}

// dataDirFor builds <root>/data/backends: "historical" carries a populated
// registry with revision history; "fresh" has none (first-ever start).
func registryIn(t *testing.T, historical bool) (reg string, want registrySnapshot) {
	root := filepath.Join(t.TempDir(), "data")
	reg = filepath.Join(root, "backends")
	require.NoError(t, os.MkdirAll(root, 0o700))
	if historical {
		want = buildRegistry(t, reg)
	}
	return reg, want
}

func TestReleaseGateMatrix(t *testing.T) {
	type scenario struct {
		name  string
		setup func(t *testing.T) (reg string, want registrySnapshot, wantRecovered bool)
		run   func(t *testing.T, e *gateEnv, want registrySnapshot)
	}
	historical := func(t *testing.T) (string, registrySnapshot, bool) {
		reg, want := registryIn(t, true)
		return reg, want, false
	}
	scenarios := []scenario{
		{"fresh data dir normal update", func(t *testing.T) (string, registrySnapshot, bool) {
			reg, w := registryIn(t, false)
			return reg, w, false
		}, func(t *testing.T, e *gateEnv, _ registrySnapshot) {
			res, err := updater.Apply(e.opts)
			require.NoError(t, err)
			require.True(t, res.Updated)
			require.Equal(t, "new", e.installed())
			require.Nil(t, e.d.res, "nothing to recover on a fresh dir")
		}},
		{"historical normal update preserves preferences", historical, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			res, err := updater.Apply(e.opts)
			require.NoError(t, err)
			require.True(t, res.Updated)
			requireRegistryPreserved(t, want, e.reopenSnapshot())
		}},
		{"delayed but eventually ready daemon", historical, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			e.d.readyAfter = 7
			res, err := updater.Apply(e.opts)
			require.NoError(t, err)
			require.True(t, res.Updated)
			require.False(t, res.RolledBack)
			require.GreaterOrEqual(t, e.d.probes, 8)
			requireRegistryPreserved(t, want, e.reopenSnapshot())
		}},
		{"daemon never becomes ready is bounded and rolls back", historical, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			e.d.readyAfter = 1 << 30
			e.opts.ReadyTimeout = 5 * time.Second
			res, err := updater.Apply(e.opts)
			e.requireNoBareConnRefused(err)
			require.True(t, res.RolledBack)
			require.Contains(t, err.Error(), "readiness timeout")
			require.Equal(t, "old", e.installed())
			requireRegistryPreserved(t, want, e.reopenSnapshot())
		}},
		{"issue 841 revision regression is recovered with backup", func(t *testing.T) (string, registrySnapshot, bool) {
			dir := filepath.Join(t.TempDir(), "data")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			reg := filepath.Join(dir, "backends")
			want := buildRegistry(t, reg)
			require.Equal(t, 34, injectDivergentWrites(t, reg, "backends", 17, detectionOnly))
			return reg, want, true
		}, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			before := treeBytes(t, e.reg)
			res, err := updater.Apply(e.opts)
			require.NoError(t, err)
			require.True(t, res.Updated)
			require.Contains(t, e.out.String(), "auto-recoverable (not blocking)")
			require.NotNil(t, e.d.res)
			require.True(t, e.d.res.Recovered)
			// the backup is a byte-identical copy of the pre-repair store...
			require.Equal(t, before, treeBytes(t, e.d.res.BackupPath))
			// ...and restores independently into a fresh dir.
			fresh := filepath.Join(t.TempDir(), "restored")
			require.NoError(t, copyDirVerified(context.Background(), e.d.res.BackupPath, fresh))
			s, rres, err := Open(fresh, Options{BackupDir: t.TempDir(), Logger: quiet().Logger})
			require.NoError(t, err)
			require.True(t, rres.Recovered)
			requireRegistryPreserved(t, want, snapshotRegistry(t, s))
			require.NoError(t, s.Close())
			requireRegistryPreserved(t, want, e.reopenSnapshot())
		}},
		{"repairable derived-index defect", func(t *testing.T) (string, registrySnapshot, bool) {
			reg, want := registryIn(t, true)
			corruptIndexes(t, reg, "backends", "role_tiers", "models")
			return reg, want, false
		}, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			rep, err := Verify(context.Background(), e.reg)
			require.NoError(t, err)
			for _, c := range rep.Collections {
				require.NotEqual(t, VerdictAmbiguous, c.Verdict, "index damage must never be ambiguous: %s", c.Name)
			}
			res, err := updater.Apply(e.opts)
			require.NoError(t, err)
			require.True(t, res.Updated)
			requireRegistryPreserved(t, want, e.reopenSnapshot())
		}},
		{"ambiguous conflict needs the operator and changes nothing", func(t *testing.T) (string, registrySnapshot, bool) {
			dir := filepath.Join(t.TempDir(), "data")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			reg := filepath.Join(dir, "backends")
			want := buildRegistry(t, reg)
			injectDivergentWrites(t, reg, "backends", 3, func(j int, id uint64, d map[string]any) {
				detectionOnly(j, id, d)
				d["tier"] = TierFree
				d["enabled"] = false
			})
			return reg, want, false
		}, func(t *testing.T, e *gateEnv, _ registrySnapshot) {
			before := treeBytes(t, e.reg)
			res, err := updater.Apply(e.opts)
			var pe *updater.PreflightError
			require.ErrorAs(t, err, &pe)
			require.False(t, res.Updated)
			require.Contains(t, err.Error(), RepairCommand, "operator is told the exact command")
			require.Equal(t, "old", e.installed(), "binary untouched")
			require.Equal(t, 0, e.d.restarts, "service untouched")
			require.Equal(t, before, treeBytes(t, e.reg), "registry bytes untouched")
			// Even if the new daemon is forced to boot, it refuses with the typed
			// error and still mutates nothing.
			_, _, oerr := Open(e.reg, quiet())
			var rre *RecoveryRequiredError
			require.ErrorAs(t, oerr, &rre)
			require.Equal(t, before, treeBytes(t, e.reg))
		}},
		{"new daemon exits before health (ambiguous slipped past preflight)", func(t *testing.T) (string, registrySnapshot, bool) {
			dir := filepath.Join(t.TempDir(), "data")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			reg := filepath.Join(dir, "backends")
			want := buildRegistry(t, reg)
			injectDivergentWrites(t, reg, "backends", 3, func(j int, id uint64, d map[string]any) {
				detectionOnly(j, id, d)
				d["tier"] = TierFree
			})
			return reg, want, false
		}, func(t *testing.T, e *gateEnv, _ registrySnapshot) {
			e.opts.Preflight = nil // simulate a registry that went bad after preflight
			before := treeBytes(t, e.reg)
			res, err := updater.Apply(e.opts)
			e.requireNoBareConnRefused(err)
			require.True(t, res.RolledBack)
			require.False(t, res.Updated)
			require.Contains(t, err.Error(), "startup failure")
			require.Contains(t, err.Error(), "warden daemon:")
			require.Contains(t, err.Error(), RepairCommand, "the daemon's own refusal carries the repair command")
			require.Equal(t, "old", e.installed())
			require.Equal(t, "1.0.0", e.d.running, "prior daemon serving again")
			require.Equal(t, before, treeBytes(t, e.reg), "failed boot mutated nothing")
		}},
		{"rollback after migration failure", historical, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			e.opts.Migrate = func() error { return errors.New("config schema 9 -> 10: boom") }
			res, err := updater.Apply(e.opts)
			e.requireNoBareConnRefused(err)
			require.True(t, res.RolledBack)
			require.Contains(t, err.Error(), "migrate")
			require.Equal(t, "old", e.installed())
			require.Equal(t, 1, e.d.restarts, "prior daemon restarted after a failed migration")
			require.Equal(t, "1.0.0", e.d.running, "prior daemon serving again")
			requireRegistryPreserved(t, want, e.reopenSnapshot())
		}},
		{"rollback after restart failure", historical, func(t *testing.T, e *gateEnv, want registrySnapshot) {
			// the new binary starts but dies; prior version must be healthy again
			require.NoError(t, os.WriteFile(filepath.Join(e.reg, "models", "seg_000001.ndjson"), []byte("garbage\n"), 0o600))
			e.opts.Preflight = nil
			res, err := updater.Apply(e.opts)
			e.requireNoBareConnRefused(err)
			require.True(t, res.RolledBack)
			require.Equal(t, "old", e.installed())
			require.Equal(t, "1.0.0", e.d.running)
			require.Contains(t, err.Error(), "warden daemon:")
		}},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			reg, want, _ := sc.setup(t)
			e := newGateEnv(t, reg)
			sc.run(t, e, want)
		})
	}
}

// Contention with systemd/manual launch: the data-dir ownership lock admits
// exactly one daemon and names the owner and the right next step; a backend
// repair refuses a foreign owner and leaves the registry untouched.
func TestReleaseGateOwnershipContention(t *testing.T) {
	for _, tc := range []struct{ launch, env, hint string }{
		{ownerlock.LaunchSystemd, "unit-1", "systemctl --user"},
		{ownerlock.LaunchManual, "", "stop"},
	} {
		t.Run(tc.launch, func(t *testing.T) {
			t.Setenv("INVOCATION_ID", tc.env)
			t.Setenv("JOURNAL_STREAM", "")
			reg, _ := registryIn(t, true)
			dataDir := filepath.Dir(reg)
			l, err := ownerlock.Acquire(dataDir, ownerlock.Info{Kind: "daemon", Version: "9.0.0"})
			require.NoError(t, err)
			defer l.Release()

			_, err = ownerlock.Acquire(dataDir, ownerlock.Info{Kind: "daemon", Version: "9.1.0"})
			var oe *ownerlock.OwnedError
			require.ErrorAs(t, err, &oe)
			require.ErrorIs(t, err, ownerlock.ErrOwned)
			require.Contains(t, oe.NextStep(), tc.hint)
			require.Contains(t, oe.NextStep(), "do not delete the lock file")
			require.Equal(t, tc.launch, oe.Owner.Launch)
		})
	}
}
