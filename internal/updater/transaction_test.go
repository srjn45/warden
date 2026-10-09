package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type txEnv struct {
	t      *testing.T
	bin    string
	staged string
	svc    *fakeSvc
	clock  *fakeClock
	out    *bytes.Buffer
	txn    *txn
}

// newTxEnv builds a transaction against a real temp-dir binary with fake
// service/probe/clock. probe receives the env so scripts can key off restarts.
func newTxEnv(t *testing.T, probe func(e *txEnv) func() (Health, error)) *txEnv {
	t.Helper()
	dir := t.TempDir()
	e := &txEnv{t: t, bin: filepath.Join(dir, "warden"), staged: filepath.Join(dir, "new"), svc: &fakeSvc{}, clock: newFakeClock(), out: &bytes.Buffer{}}
	require.NoError(t, os.WriteFile(e.bin, []byte("old"), 0o755))
	require.NoError(t, os.WriteFile(e.staged, []byte("new"), 0o755))
	opts := Options{CurrentVersion: "1.0.0", InstallBin: e.bin, Stdout: e.out, ReadyTimeout: 10 * time.Second}
	p := &fakeProbe{fn: probe(e)}
	e.txn = &txn{opts: opts, svc: e.svc, probe: p, inst: fsInstaller{bin: e.bin}, clock: e.clock, target: "2.0.0"}
	return e
}

func (e *txEnv) run() (bool, error) {
	e.txn.capture(context.Background())
	return e.txn.apply(context.Background(), e.staged)
}

func (e *txEnv) binary() string { b, _ := os.ReadFile(e.bin); return string(b) }

func ok(v string) (Health, error) { return Health{Status: "ok", Version: v}, nil }

func TestTxnCleanUpdate(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) {
			if e.svc.count() == 0 {
				return ok("1.0.0")
			}
			return ok("2.0.0")
		}
	})
	rb, err := e.run()
	require.NoError(t, err)
	require.False(t, rb)
	require.Equal(t, "new", e.binary())
	require.Equal(t, 1, e.svc.count())
	require.NoFileExists(t, e.bin+".bak")
}

func TestTxnDelayedReady(t *testing.T) {
	calls := 0
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) {
			if e.svc.count() == 0 {
				return ok("1.0.0")
			}
			calls++
			switch {
			case calls < 3:
				return Health{}, errors.New("connection refused")
			case calls < 5:
				return ok("1.0.0") // old process still answering / not yet swapped
			}
			return ok("2.0.0")
		}
	})
	_, err := e.run()
	require.NoError(t, err)
	require.Equal(t, "new", e.binary())
	require.Equal(t, 1, e.svc.count(), "a slow daemon must not trigger a second restart")
}

func TestTxnStartupFailureSurfacesDiagnosis(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) {
			if e.svc.count() == 1 {
				return Health{}, errors.New("connection refused")
			}
			return ok("1.0.0")
		}
	})
	e.svc.diag = "backend registry: id 1 update has revision 68 after revision 70\nrun: warden repair backends"
	e.svc.exited = func(n int) bool { return n == 1 }
	rb, err := e.run()
	require.Error(t, err)
	require.True(t, rb)
	var su *StartupFailureError
	require.ErrorAs(t, err, &su)
	var fe *FailureError
	require.ErrorAs(t, err, &fe)
	require.NoError(t, fe.Rollback)
	require.Contains(t, err.Error(), "startup failure")
	require.Contains(t, err.Error(), "revision 68 after revision 70")
	require.NotContains(t, err.Error(), "connection refused")
	require.Contains(t, err.Error(), "restored v1.0.0")
	require.Equal(t, "old", e.binary())
	require.Equal(t, 2, e.svc.count(), "previous service explicitly restarted")
}

func TestTxnWrongVersionRollsBack(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") } // never advertises 2.0.0
	})
	rb, err := e.run()
	var wv *WrongVersionError
	require.ErrorAs(t, err, &wv)
	require.Equal(t, "2.0.0", wv.Want)
	require.Equal(t, "1.0.0", wv.Got)
	require.True(t, rb)
	require.Contains(t, err.Error(), "wrong version")
	require.Equal(t, "old", e.binary())
}

func TestTxnReadinessTimeoutBounded(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) {
			if e.svc.count() == 1 {
				return Health{Status: "starting"}, nil
			}
			return ok("1.0.0")
		}
	})
	start := e.clock.Now()
	rb, err := e.run()
	var rt *ReadinessTimeoutError
	require.ErrorAs(t, err, &rt)
	require.True(t, rb)
	require.Contains(t, err.Error(), "readiness timeout")
	require.Contains(t, err.Error(), `health status "starting"`)
	require.GreaterOrEqual(t, e.clock.Now().Sub(start), 10*time.Second)
	require.Less(t, e.clock.Now().Sub(start), 14*time.Second, "deadline is overall, not per-attempt")
	require.Equal(t, "old", e.binary())
}

func TestTxnRollbackFailureReportsBoth(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) {
			if e.svc.count() == 0 {
				return ok("1.0.0")
			}
			return Health{}, errors.New("connection refused") // old binary never recovers either
		}
	})
	e.txn.opts.ReadyTimeout = 3 * time.Second
	rb, err := e.run()
	require.Error(t, err)
	require.False(t, rb)
	var rbe *RollbackError
	require.ErrorAs(t, err, &rbe)
	var rt *ReadinessTimeoutError
	require.ErrorAs(t, err, &rt, "original failure is still reported")
	require.Contains(t, err.Error(), "ROLLBACK FAILED")
	require.Equal(t, "old", e.binary(), "binary was restored even though service is unhealthy")
}

func TestTxnRollbackRestartFailure(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") }
	})
	e.svc.restartErr = func(n int) error {
		if n == 2 {
			return errors.New("unit masked")
		}
		return nil
	}
	_, err := e.run()
	var rbe *RollbackError
	require.ErrorAs(t, err, &rbe)
	require.Contains(t, err.Error(), "unit masked")
}

func TestTxnRestartCommandFailureIncludesDiagnostics(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") }
	})
	e.svc.diag = "fatal: store refused"
	e.svc.restartErr = func(n int) error {
		if n == 1 {
			return errors.New("systemctl restart: exit 1")
		}
		return nil
	}
	_, err := e.run()
	var su *StartupFailureError
	require.ErrorAs(t, err, &su)
	require.Contains(t, err.Error(), "fatal: store refused")
	require.Contains(t, err.Error(), "systemctl restart: exit 1")
}

func TestTxnMigrateFailureRollsBackWithoutRestart(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") }
	})
	e.txn.opts.Migrate = func() error { return fmt.Errorf("bad config") }
	rb, err := e.run()
	require.Error(t, err)
	require.True(t, rb)
	require.Equal(t, "old", e.binary())
	require.Equal(t, 0, e.svc.count(), "daemon was never restarted; nothing to restart back")
}

func TestTxnNoServiceManager(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") }
	})
	e.svc.restartErr = func(int) error { return ErrNoServiceManager }
	rb, err := e.run()
	require.NoError(t, err)
	require.False(t, rb)
	require.Equal(t, "new", e.binary())
	require.Contains(t, e.out.String(), "restart the warden daemon manually")
}

func TestTxnPreflightBlockerNoSwap(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") }
	})
	e.txn.opts.Preflight = func(context.Context) (PreflightResult, error) {
		return PreflightResult{
			Blockers:      []string{"backends: conflict (history conflict)"},
			Notes:         []string{"models: index damage"},
			RepairCommand: "warden repair backends",
		}, nil
	}
	e.txn.capture(context.Background())
	err := e.txn.preflight(context.Background())
	var pe *PreflightError
	require.ErrorAs(t, err, &pe)
	require.Contains(t, err.Error(), "warden repair backends")
	require.Contains(t, e.out.String(), "auto-recoverable (not blocking): models")
	require.Equal(t, "old", e.binary())
	require.Equal(t, 0, e.svc.count())
}

func TestTxnPreflightRecoverableNotBlocking(t *testing.T) {
	e := newTxEnv(t, func(e *txEnv) func() (Health, error) {
		return func() (Health, error) { return ok("1.0.0") }
	})
	e.txn.opts.Preflight = func(context.Context) (PreflightResult, error) {
		return PreflightResult{Notes: []string{"backends: stale revisions"}}, nil
	}
	require.NoError(t, e.txn.preflight(context.Background()))
	require.Contains(t, e.out.String(), "stale revisions")
}

// Apply-level: a preflight blocker stops before any download or swap.
func TestApplyPreflightBlockerBeforeDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected download request %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	bin := filepath.Join(home, "warden")
	require.NoError(t, os.WriteFile(bin, []byte("old"), 0o755))
	svc := &fakeSvc{}
	_, err := Apply(Options{
		CurrentVersion: "1.0.0", TargetVersion: "2.0.0", GOOS: "linux", GOARCH: "amd64",
		InstallBin: bin, StagingDir: filepath.Join(home, "tmp"), AssetBase: srv.URL,
		Service: svc, Prober: &fakeProbe{fn: func() (Health, error) { return ok("1.0.0") }}, Clock: newFakeClock(),
		Preflight: func(context.Context) (PreflightResult, error) {
			return PreflightResult{Blockers: []string{"backends: x"}, RepairCommand: "warden repair backends"}, nil
		},
	})
	var pe *PreflightError
	require.ErrorAs(t, err, &pe)
	b, _ := os.ReadFile(bin)
	require.Equal(t, "old", string(b))
	require.Equal(t, 0, svc.count())
}
