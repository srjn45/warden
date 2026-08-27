package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// recordingWaker captures the OwnerWaker calls the executor makes, so a test can
// assert exactly when (and with what body) a delegated pipeline's owner is woken.
type recordingWaker struct {
	mu    sync.Mutex
	calls []wakeCall
}

type wakeCall struct{ owner, body string }

func (w *recordingWaker) wake(owner, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, wakeCall{owner, body})
}

func (w *recordingWaker) snapshot() []wakeCall {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wakeCall(nil), w.calls...)
}

func delegated(jobs ...pipeline.Job) *pipeline.Pipeline {
	return &pipeline.Pipeline{ID: "d", Name: "d", Repo: "/r", Status: pipeline.StatusPending,
		OwnerID: "orch", NotifyOwner: true, Jobs: jobs}
}

// The owner is woken at a declared callback point when that job emits, and once
// more when the pipeline completes — never between callbacks.
func TestDelegatedWakeAtCallbackAndCompletion(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	w := &recordingWaker{}
	e.SetOwnerWaker(w.wake)
	ps.Create(delegated(
		pipeline.Job{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobPending, Callback: true},
		pipeline.Job{ID: "b", Prompt: "y", DependsOn: []string{"a"}, Worktree: "none", Status: pipeline.JobPending},
	))
	ctx := context.Background()
	if err := e.Reconcile(ctx, "d"); err != nil { // spawn a; running → no wake yet
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(w.snapshot()); got != 0 {
		t.Fatalf("no wake on spawn, got %d", got)
	}
	// a is a callback point → wake on its emit; b is still to run, so no completion.
	if err := e.Emit(ctx, "d", "a", "a done"); err != nil {
		t.Fatalf("Emit a: %v", err)
	}
	calls := w.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 wake after callback emit, got %d: %+v", len(calls), calls)
	}
	if calls[0].owner != "orch" || !strings.Contains(calls[0].body, `callback point "a"`) {
		t.Fatalf("callback wake malformed: %+v", calls[0])
	}
	// b is not a callback point → its emit wakes only via the completion transition.
	if err := e.Emit(ctx, "d", "b", "b done"); err != nil {
		t.Fatalf("Emit b: %v", err)
	}
	calls = w.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want 2 wakes after completion, got %d: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[1].body, "completed") {
		t.Fatalf("completion wake malformed: %+v", calls[1])
	}
}

// A pure DAG (no callback points) wakes the owner exactly once — on completion.
func TestDelegatedPureDAGWakesOnceAtEnd(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	w := &recordingWaker{}
	e.SetOwnerWaker(w.wake)
	ps.Create(delegated(
		pipeline.Job{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobPending},
		pipeline.Job{ID: "b", Prompt: "y", DependsOn: []string{"a"}, Worktree: "none", Status: pipeline.JobPending},
	))
	ctx := context.Background()
	e.Reconcile(ctx, "d")
	e.Emit(ctx, "d", "a", "a done") // not a callback → no wake, spawns b
	if got := len(w.snapshot()); got != 0 {
		t.Fatalf("pure DAG must not wake mid-run, got %d", got)
	}
	e.Emit(ctx, "d", "b", "b done") // completes → single wake
	calls := w.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "completed") {
		t.Fatalf("want 1 completion wake, got %+v", calls)
	}
}

// An owned but unsubscribed pipeline (notify_owner=false) wakes no one, even at a
// callback-marked job — the DAG behaves exactly as before A4.
func TestDelegatedNoWakeWithoutSubscription(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	w := &recordingWaker{}
	e.SetOwnerWaker(w.wake)
	p := delegated(pipeline.Job{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobPending, Callback: true})
	p.NotifyOwner = false
	ps.Create(p)
	ctx := context.Background()
	e.Reconcile(ctx, "d")
	e.Emit(ctx, "d", "a", "done")
	if got := len(w.snapshot()); got != 0 {
		t.Fatalf("unsubscribed pipeline must not wake, got %d", got)
	}
}

// A subscribed pipeline with an empty owner link wakes no one (nothing to wake).
func TestDelegatedNoWakeWithoutOwner(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	w := &recordingWaker{}
	e.SetOwnerWaker(w.wake)
	p := delegated(pipeline.Job{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobPending, Callback: true})
	p.OwnerID = ""
	ps.Create(p)
	ctx := context.Background()
	e.Reconcile(ctx, "d")
	e.Emit(ctx, "d", "a", "done")
	if got := len(w.snapshot()); got != 0 {
		t.Fatalf("owner-less pipeline must not wake, got %d", got)
	}
}

// A stall (an unhandled job failure) wakes the owner once so it is released from
// wait_for_message rather than left to time out — and a repeated reconcile of the
// still-stalled pipeline does not re-wake.
func TestDelegatedWakeOnStallOnce(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	w := &recordingWaker{}
	e.SetOwnerWaker(w.wake)
	ps.Create(delegated(
		pipeline.Job{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobPending},
		pipeline.Job{ID: "b", Prompt: "y", DependsOn: []string{"a"}, Worktree: "none", Status: pipeline.JobPending},
	))
	ctx := context.Background()
	e.Reconcile(ctx, "d") // spawn a
	sess, _ := ss.Get(ctx, "d-a")
	e.OnTransition(sess, store.StatusWorking, store.StatusErrored) // a fails → stalled
	calls := w.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "stalled") {
		t.Fatalf("want 1 stall wake, got %+v", calls)
	}
	if err := e.Reconcile(ctx, "d"); err != nil { // still stalled → must not re-wake
		t.Fatalf("Reconcile: %v", err)
	}
	if got := len(w.snapshot()); got != 1 {
		t.Fatalf("stall wake must fire exactly once, got %d", got)
	}
}

// The completion wake fires exactly once: reconciling an already-done pipeline
// again does not re-notify the owner.
func TestDelegatedCompletionWakeIdempotent(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	w := &recordingWaker{}
	e.SetOwnerWaker(w.wake)
	ps.Create(delegated(pipeline.Job{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobPending}))
	ctx := context.Background()
	e.Reconcile(ctx, "d")
	e.Emit(ctx, "d", "a", "a done") // completes → 1 wake
	e.Reconcile(ctx, "d")           // already done → guard bails, no wake
	if got := len(w.snapshot()); got != 1 {
		t.Fatalf("completion wake must fire exactly once, got %d", got)
	}
}
