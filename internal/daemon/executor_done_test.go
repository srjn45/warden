package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/pipeline"
)

// TestDoneSuccessMarksDoneAndFansOut: a success done-signal records the self-
// report, marks the job done, and unblocks the dependent — same terminal state
// as Emit, so warden closes the job in one shot.
func TestDoneSuccessMarksDoneAndFansOut(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // spawns a

	if err := e.Done(context.Background(), "p", "a", "success", "did the thing"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobDone {
		t.Fatalf("a should be done, got %s", got.Job("a").Status)
	}
	if got.Job("a").Result != "success" || got.Job("a").Summary != "did the thing" {
		t.Fatalf("self-report not recorded: result=%q summary=%q", got.Job("a").Result, got.Job("a").Summary)
	}
	if got.Job("a").Output != "did the thing" {
		t.Fatalf("summary should become the downstream handoff, got %q", got.Job("a").Output)
	}
	if got.Job("b").Status != pipeline.JobRunning {
		t.Fatalf("dependent b should now run, got %s", got.Job("b").Status)
	}
}

// TestDoneEmptyStatusDefaultsSuccess: an omitted status is treated as success.
func TestDoneEmptyStatusDefaultsSuccess(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")

	if err := e.Done(context.Background(), "p", "a", "", "summary"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobDone {
		t.Fatalf("empty status should default to success/done, got %s", got.Job("a").Status)
	}
	if got.Job("a").Result != "success" {
		t.Fatalf("result should normalize to success, got %q", got.Job("a").Result)
	}
}

// TestDoneFailureParksNeedsAttention: a self-declared failure records the report
// and parks the job as needs_attention WITHOUT fanning out to dependents.
func TestDoneFailureParksNeedsAttention(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")

	if err := e.Done(context.Background(), "p", "a", "failure", "hit a wall"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobNeedsAttention {
		t.Fatalf("failed job should be needs_attention, got %s", got.Job("a").Status)
	}
	if got.Job("a").Result != "failure" || got.Job("a").Summary != "hit a wall" {
		t.Fatalf("self-report not recorded: result=%q summary=%q", got.Job("a").Result, got.Job("a").Summary)
	}
	if got.Job("b").Status != pipeline.JobPending {
		t.Fatalf("dependent b must NOT run off a failed job, got %s", got.Job("b").Status)
	}
}

// TestDoneBlockedIsIdempotent: a lingering sentinel captured every poller tick
// must record the completion exactly once — the second call is a no-op.
func TestDoneBlockedIsIdempotent(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")

	if err := e.Done(context.Background(), "p", "a", "blocked", "waiting on review"); err != nil {
		t.Fatalf("first Done: %v", err)
	}
	// Second capture of the same sentinel: no error, no state churn.
	if err := e.Done(context.Background(), "p", "a", "blocked", "waiting on review"); err != nil {
		t.Fatalf("second Done should be a no-op, got %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobNeedsAttention {
		t.Fatalf("a should still be needs_attention, got %s", got.Job("a").Status)
	}
}

// TestDoneAfterCompletionIsNoop: a done-signal for an already-finalized job (its
// self-report recorded) is a harmless no-op, not a resurrection.
func TestDoneAfterCompletionIsNoop(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")
	if err := e.Done(context.Background(), "p", "a", "success", "one"); err != nil {
		t.Fatalf("Done: %v", err)
	}
	// a is done + reaped; a second success signal (e.g. sentinel still on pane)
	// finds it already terminal with a Result set → no-op.
	if err := e.Done(context.Background(), "p", "a", "success", "two"); err != nil {
		t.Fatalf("second Done should be a no-op, got %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Summary != "one" {
		t.Fatalf("summary should not be overwritten by a late duplicate, got %q", got.Job("a").Summary)
	}
}

// TestDoneUnknownJob surfaces ErrJobNotFound so the route can map it to 404.
func TestDoneUnknownJob(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")
	if err := e.Done(context.Background(), "p", "nope", "success", "x"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

// TestDonePendingJobRejected: a job that never started (still pending) cannot be
// self-reported done — that would spawn descendants off work that never ran.
func TestDonePendingJobRejected(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // a runs, b stays pending
	if err := e.Done(context.Background(), "p", "b", "success", "x"); !errors.Is(err, ErrJobNotRunning) {
		t.Fatalf("want ErrJobNotRunning for a pending job, got %v", err)
	}
}
