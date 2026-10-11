package crashtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/dbtx/workflows"
)

// faultEngine is the NewWithEngine seam: it delegates to the real engine and
// lets a case replace CommitXTx / XTxStatus to provoke failure classes that
// cannot be forced for real.
type faultEngine struct {
	dbtx.Engine
	mu      sync.Mutex
	calls   int
	faulted bool // a commit fault was injected; status overrides apply from now on
	commit  func(call int, f *faultEngine, key string, ops []engine.XTxOp) (*engine.XTxResult, error)
	status  func(f *faultEngine, key string) (engine.XTxStatus, string)
}

func (f *faultEngine) CommitXTx(key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.commit == nil {
		return f.Engine.CommitXTx(key, ops)
	}
	return f.commit(n, f, key, ops)
}

func (f *faultEngine) XTxStatus(k string) (engine.XTxStatus, string) {
	f.mu.Lock()
	faulted := f.faulted
	f.mu.Unlock()
	if f.status != nil && faulted {
		return f.status(f, k)
	}
	return f.Engine.XTxStatus(k)
}

func (f *faultEngine) mark() { f.mu.Lock(); f.faulted = true; f.mu.Unlock() }
func (f *faultEngine) nCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func xerr(sentinel error) error {
	return &engine.XTxError{Err: sentinel, Cause: errors.New("injected")}
}

// failFirst fails the first n commit calls (nothing applied), then delegates.
func failFirst(n int, err error) func(int, *faultEngine, string, []engine.XTxOp) (*engine.XTxResult, error) {
	return func(call int, f *faultEngine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		if call <= n {
			f.mark()
			return nil, err
		}
		return f.Engine.CommitXTx(key, ops)
	}
}

const forever = 1 << 30

type faultCase struct {
	name       string
	commit     func(int, *faultEngine, string, []engine.XTxOp) (*engine.XTxResult, error)
	status     func(*faultEngine, string) (engine.XTxStatus, string)
	opts       []dbtx.Option
	ctxTimeout time.Duration

	wantOK        bool // Run returns nil and the state is POST
	wantClass     dbtx.Class
	wantOutcome   dbtx.Outcome
	wantRetryable bool // *Error.SafeToRetry
	wantCalls     int  // exact engine CommitXTx calls (0 = unchecked)
	lateCommit    bool // release() lets a blocked commit land afterwards
}

func faultCases() []faultCase {
	aborted := func(*faultEngine, string) (engine.XTxStatus, string) { return engine.XTxAborted, "" }
	unknown := func(*faultEngine, string) (engine.XTxStatus, string) { return engine.XTxUnknown, "" }
	pending := func(*faultEngine, string) (engine.XTxStatus, string) { return engine.XTxPending, "" }
	// applied really commits, then reports the ack as lost.
	applied := func(call int, f *faultEngine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
		if _, err := f.Engine.CommitXTx(key, ops); err != nil {
			return nil, err
		}
		f.mark()
		return nil, xerr(engine.ErrXTxOutcomeUnknown)
	}
	return []faultCase{
		{name: "conflict-once", commit: failFirst(1, xerr(engine.ErrXTxConflict)), wantOK: true, wantCalls: 2},
		{name: "conflict-always", commit: failFirst(forever, xerr(engine.ErrXTxConflict)), wantClass: dbtx.ClassConflict, wantRetryable: true, wantCalls: 4},
		{name: "exhausted-once", commit: failFirst(1, xerr(engine.ErrResourceExhausted)), wantOK: true, wantCalls: 2},
		{name: "exhausted-always", commit: failFirst(forever, xerr(engine.ErrResourceExhausted)), wantClass: dbtx.ClassExhausted, wantRetryable: true, wantCalls: 4},
		{name: "durability-once", commit: failFirst(1, xerr(engine.ErrXTxDurability)), wantOK: true, wantCalls: 2},
		{name: "durability-always", commit: failFirst(forever, xerr(engine.ErrXTxDurability)), wantClass: dbtx.ClassDurability, wantRetryable: true, wantCalls: 4},
		{name: "too-large", commit: failFirst(forever, xerr(engine.ErrXTxTooLarge)), wantClass: dbtx.ClassTooLarge, wantCalls: 1},
		{name: "collection-not-found", commit: failFirst(forever, xerr(engine.ErrCollectionNotFound)), wantClass: dbtx.ClassCollectionNotFound, wantCalls: 1},
		{name: "untyped", commit: failFirst(forever, errors.New("disk on fire")), wantClass: dbtx.ClassInternal, wantCalls: 1},
		{name: "outcome-unknown-resolves-committed", commit: applied, wantOK: true, wantCalls: 1},
		{name: "outcome-unknown-resolves-aborted-then-retry-ok",
			commit: failFirst(1, xerr(engine.ErrXTxOutcomeUnknown)), status: aborted, wantOK: true, wantCalls: 2},
		{name: "outcome-unknown-aborted-always",
			commit: failFirst(forever, xerr(engine.ErrXTxOutcomeUnknown)), status: aborted,
			wantClass: dbtx.ClassOutcomeUnknown, wantOutcome: dbtx.OutcomeAborted, wantRetryable: true, wantCalls: 4},
		{name: "outcome-unknown-resolves-unknown",
			commit: failFirst(forever, xerr(engine.ErrXTxOutcomeUnknown)), status: unknown,
			wantClass: dbtx.ClassOutcomeUnknown, wantOutcome: dbtx.OutcomeUnknown, wantRetryable: true, wantCalls: 4},
		{name: "outcome-unknown-pending",
			commit: failFirst(forever, xerr(engine.ErrXTxOutcomeUnknown)), status: pending,
			opts:      []dbtx.Option{dbtx.WithResolveWait(30 * time.Millisecond)},
			wantClass: dbtx.ClassOutcomeUnknown, wantOutcome: dbtx.OutcomePending, wantCalls: 1},
		{name: "in-progress-other-executor",
			commit: failFirst(forever, xerr(engine.ErrXTxInProgress)), status: pending,
			opts:      []dbtx.Option{dbtx.WithResolveWait(30 * time.Millisecond)},
			wantClass: dbtx.ClassOutcomeUnknown, wantOutcome: dbtx.OutcomePending, wantCalls: 1},
		{name: "deadline-then-late-commit", ctxTimeout: 40 * time.Millisecond, lateCommit: true,
			opts:      []dbtx.Option{dbtx.WithDeadlineGrace(10 * time.Millisecond)},
			wantClass: dbtx.ClassOutcomeUnknown, wantOutcome: dbtx.OutcomeUnknown, wantCalls: 1},
	}
}

func TestInjectedCommitFaults(t *testing.T) {
	for _, w := range workflows.All() {
		w := w
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			b := prepare(t, w)
			for _, fc := range faultCases() {
				fc := fc
				t.Run(fc.name, func(t *testing.T) {
					dir := b.fresh(t)
					fe := &faultEngine{commit: fc.commit, status: fc.status}
					release := make(chan struct{})
					var cancelLateCommit context.CancelFunc
					if fc.lateCommit {
						fe.commit = func(call int, f *faultEngine, key string, ops []engine.XTxOp) (*engine.XTxResult, error) {
							cancelLateCommit()
							<-release
							return f.Engine.CommitXTx(key, ops)
						}
					}
					s := mustOpen(t, dir, func(in dbtx.Engine) dbtx.Engine { fe.Engine = in; return fe }, fc.opts...)
					ctx := bg
					if fc.lateCommit {
						ctx, cancelLateCommit = context.WithCancel(bg)
						defer cancelLateCommit()
					} else if fc.ctxTimeout > 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(bg, fc.ctxTimeout)
						defer cancel()
					}
					err := w.Run(ctx, s.e)
					snap := snapshot(t, s.db)
					st := b.classify(snap)
					if err := checkAll(s.db); err != nil {
						t.Fatalf("checker right after fault: %v", err)
					}
					if fc.wantCalls != 0 && fe.nCalls() != fc.wantCalls {
						t.Errorf("engine CommitXTx calls = %d, want %d", fe.nCalls(), fc.wantCalls)
					}
					switch {
					case fc.wantOK:
						if err != nil {
							t.Fatalf("Run: %v", err)
						}
						if st != statePost {
							t.Fatalf("success but state %s", st)
						}
					default:
						if err == nil {
							t.Fatalf("silent success under injected %s (state %s)", fc.name, st)
						}
						var be *dbtx.Error
						if !errors.As(err, &be) {
							t.Fatalf("untyped error leaked: %T %v", err, err)
						}
						if be.Class != fc.wantClass {
							t.Fatalf("class = %s, want %s (%v)", be.Class, fc.wantClass, err)
						}
						if be.Outcome != fc.wantOutcome {
							t.Errorf("outcome = %q, want %q", be.Outcome, fc.wantOutcome)
						}
						if be.SafeToRetry != fc.wantRetryable {
							t.Errorf("SafeToRetry = %v, want %v", be.SafeToRetry, fc.wantRetryable)
						}
						if st != statePre {
							t.Fatalf("failed commit (%s) left state %s", be.Class, st)
						}
						if be.Class == dbtx.ClassOutcomeUnknown && be.Key == "" {
							t.Errorf("outcome_unknown without a commit key")
						}
						if fc.wantCalls > 1 && be.Attempts != fc.wantCalls {
							t.Errorf("Attempts = %d, want %d", be.Attempts, fc.wantCalls)
						}
					}
					if fc.lateCommit {
						// The in-flight commit may still land; it was flagged
						// outcome_unknown, so either end state is legal — but
						// it must be exactly one of them.
						close(release)
						deadline := time.Now().Add(5 * time.Second)
						for time.Now().Before(deadline) {
							if o, _ := s.b.Outcome(b.key); o == dbtx.OutcomeCommitted {
								break
							}
							time.Sleep(5 * time.Millisecond)
						}
						if st := b.classify(snapshot(t, s.db)); st != statePost {
							t.Fatalf("late commit finished but state %s", st)
						}
					}
					var keys []string
					for _, ev := range s.rec.events() {
						if ev.Outcome == "committed" {
							keys = append(keys, ev.Key)
						}
					}
					s.close()
					b.verifyReopened(t, dir, w.Name+"/"+fc.name, keys...)
				})
			}
		})
	}
}

// ---- conflicts (scenario 4) -----------------------------------------------

// interfered returns the snapshot of seedDir after n competing writes.
func (b *base) interfered(t *testing.T, n int, thenRun bool) string {
	t.Helper()
	d := b.fresh(t)
	s := mustOpen(t, d, nil)
	defer s.close()
	for i := 0; i < n; i++ {
		if err := b.w.Interfere(s.e); err != nil {
			t.Fatalf("interfere: %v", err)
		}
	}
	if thenRun {
		if err := b.w.Run(bg, s.e); err != nil {
			t.Fatalf("run after interference: %v", err)
		}
	}
	return snapshot(t, s.db)
}

func TestConflictsRaceBetweenRebuildAndCommit(t *testing.T) {
	for _, w := range workflows.All() {
		w := w
		t.Run(w.Name, func(t *testing.T) {
			t.Parallel()
			b := prepare(t, w)

			t.Run("once-then-converges", func(t *testing.T) {
				want := b.interfered(t, 1, true)
				dir := b.fresh(t)
				s := mustOpen(t, dir, nil)
				attempts := 0
				s.e.BeforeCommit = func(a int) error {
					attempts = a
					if a == 1 {
						return w.Interfere(s.e)
					}
					return nil
				}
				if err := w.Run(bg, s.e); err != nil {
					t.Fatalf("run: %v", err)
				}
				if attempts != 2 {
					t.Fatalf("attempts = %d, want 2 (conflict, rebuild)", attempts)
				}
				if got := snapshot(t, s.db); got != want {
					t.Fatalf("converged state differs from interference+run reference")
				}
				if err := checkAll(s.db); err != nil {
					t.Fatal(err)
				}
				s.close()
				assertStable(t, dir, want, "converged")
			})

			t.Run("single-attempt-typed-conflict-no-partial", func(t *testing.T) {
				want := b.interfered(t, 1, false)
				dir := b.fresh(t)
				s := mustOpen(t, dir, nil, dbtx.WithMaxAttempts(1))
				s.e.BeforeCommit = func(int) error { return w.Interfere(s.e) }
				err := w.Run(bg, s.e)
				var be *dbtx.Error
				if !errors.As(err, &be) || be.Class != dbtx.ClassConflict || !be.SafeToRetry {
					t.Fatalf("err = %v, want typed retryable conflict", err)
				}
				if got := snapshot(t, s.db); got != want {
					t.Fatalf("conflicted commit left partial state (not exactly pre+interference)")
				}
				if err := checkAll(s.db); err != nil {
					t.Fatal(err)
				}
				// The key must not be recorded COMMITTED.
				if o, _ := s.b.Outcome(b.key); o == dbtx.OutcomeCommitted {
					t.Fatalf("Outcome(key)=COMMITTED after a conflicted commit")
				}
				s.close()
				assertStable(t, dir, want, "conflicted")
			})

			t.Run("every-attempt-exhausts-typed", func(t *testing.T) {
				want := b.interfered(t, 4, false)
				dir := b.fresh(t)
				s := mustOpen(t, dir, nil, dbtx.WithMaxAttempts(4))
				s.e.BeforeCommit = func(int) error { return w.Interfere(s.e) }
				err := w.Run(bg, s.e)
				var be *dbtx.Error
				if !errors.As(err, &be) || be.Class != dbtx.ClassConflict || be.Attempts != 4 {
					t.Fatalf("err = %v, want conflict after 4 attempts", err)
				}
				if got := snapshot(t, s.db); got != want {
					t.Fatalf("exhausted retries left partial state")
				}
				if err := checkAll(s.db); err != nil {
					t.Fatal(err)
				}
				// Once the interference stops, the same workflow converges.
				s.e.BeforeCommit = nil
				if err := w.Run(bg, s.e); err != nil {
					t.Fatalf("run after storm: %v", err)
				}
				if err := checkAll(s.db); err != nil {
					t.Fatal(err)
				}
				s.close()
				mustOpen(t, dir, nil).close()
			})
		})
	}
}

// assertStable reopens dir several times and requires a stable snapshot that
// equals want, with every checker passing.
func assertStable(t *testing.T, dir, want, label string) {
	t.Helper()
	for i := 0; i < scale(2, 5); i++ {
		s, err := openStack(dir, nil)
		if err != nil {
			t.Fatalf("%s: reopen #%d: %v", label, i+1, err)
		}
		got := snapshot(t, s.db)
		if got != want {
			s.close()
			t.Fatalf("%s: state drifted on reopen #%d", label, i+1)
		}
		if err := checkAll(s.db); err != nil {
			s.close()
			t.Fatalf("%s: checker after reopen #%d: %v", label, i+1, err)
		}
		s.close()
	}
}
