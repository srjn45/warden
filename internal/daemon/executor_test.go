package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
)

func newTestExecutor(t *testing.T) (*Executor, *pipeline.Store, *fakeStore) {
	t.Helper()
	ps, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("pipeline.NewStore: %v", err)
	}
	cs, err := ctxstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("ctxstore.New: %v", err)
	}
	ss := newFakeStore()
	e := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	return e, ps, ss
}

func chain() *pipeline.Pipeline {
	return &pipeline.Pipeline{ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusPending,
		Jobs: []pipeline.Job{
			{ID: "a", Prompt: "first", Worktree: "none", Status: pipeline.JobPending},
			{ID: "b", Prompt: "second", DependsOn: []string{"a"}, Worktree: "from:a", Status: pipeline.JobPending},
		}}
}

func TestReconcileSpawnsRootOnly(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	if err := e.Reconcile(context.Background(), "p"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobRunning || got.Job("a").SessionID != "p-a" {
		t.Fatalf("root a should be running: %+v", got.Job("a"))
	}
	if got.Job("b").Status != pipeline.JobPending {
		t.Fatalf("b should still be pending")
	}
	if got.Status != pipeline.StatusRunning {
		t.Fatalf("pipeline status %s", got.Status)
	}
	// the spawned job's session was inserted, with the back-ref.
	sess, err := ss.Get(context.Background(), "p-a")
	if err != nil || sess.PipelineID != "p" || sess.JobID != "a" {
		t.Fatalf("session not inserted with back-ref: %+v err=%v", sess, err)
	}
}

func TestReconcileUnblocksOnEmittedOutput(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // spawns a
	// simulate a's emit: mark done with output + branch.
	ps.Update("p", func(p *pipeline.Pipeline) {
		j := p.Job("a")
		j.Status = pipeline.JobDone
		j.Output = "done with a"
		j.Branch = "" // none-mode job has no branch
	})
	if err := e.Reconcile(context.Background(), "p"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("b").Status != pipeline.JobRunning || got.Job("b").SessionID != "p-b" {
		t.Fatalf("b should now be running: %+v", got.Job("b"))
	}
}

func TestOnTransitionFailsJobAndSkipsDescendants(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // a running, session p-a inserted
	// the running session errors out.
	sess, _ := ss.Get(context.Background(), "p-a")
	e.OnTransition(sess, store.StatusWorking, store.StatusErrored)
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobFailed {
		t.Fatalf("a should be failed, got %s", got.Job("a").Status)
	}
	if got.Job("b").Status != pipeline.JobSkipped {
		t.Fatalf("b (descendant) should be skipped, got %s", got.Job("b").Status)
	}
	if got.Status != pipeline.StatusStalled {
		t.Fatalf("pipeline should be stalled, got %s", got.Status)
	}
}

func TestReconcileConcurrentNoDoubleSpawn(t *testing.T) {
	// Concurrent triggers (HTTP emit + poller OnTransition) must not both spawn
	// the same ready job — which would orphan a live agent and falsely stall.
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = e.Reconcile(context.Background(), "p") }()
	}
	wg.Wait()
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("root a should be running exactly once, got %s", got.Job("a").Status)
	}
	if got.Status == pipeline.StatusStalled {
		t.Fatalf("pipeline must not be stalled by a spawn race")
	}
	if _, err := ss.Get(context.Background(), "p-a"); err != nil {
		t.Fatalf("session p-a should exist exactly once: %v", err)
	}
}

func TestEditJobOnlyWhenPending(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())

	newPrompt := "do it differently"
	newHandoff := "the branch name"
	if err := e.EditJob("p", "a", &newPrompt, &newHandoff); err != nil {
		t.Fatalf("EditJob pending: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Prompt != "do it differently" || got.Job("a").Handoff != "the branch name" {
		t.Fatalf("edit not applied: %+v", got.Job("a"))
	}

	// only one field provided → the other is untouched.
	onlyPrompt := "again"
	e.EditJob("p", "a", &onlyPrompt, nil)
	got, _ = ps.Get("p")
	if got.Job("a").Prompt != "again" || got.Job("a").Handoff != "the branch name" {
		t.Fatalf("partial edit wrong: %+v", got.Job("a"))
	}

	// running job → rejected.
	ps.Update("p", func(p *pipeline.Pipeline) { p.Job("a").Status = pipeline.JobRunning })
	if err := e.EditJob("p", "a", &onlyPrompt, nil); !errors.Is(err, ErrJobNotPending) {
		t.Fatalf("editing a running job should fail with ErrJobNotPending, got %v", err)
	}

	// unknown job → ErrJobNotFound.
	if err := e.EditJob("p", "ghost", &onlyPrompt, nil); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestRetryResetsFailedJobAndReopensDescendants(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain()) // a -> b
	// Put the pipeline in a stalled state: a failed (with a stale session), b skipped.
	ss.Insert(context.Background(), &store.Session{ID: "p-a", TmuxSession: "p-a", PipelineID: "p", JobID: "a", Status: store.StatusOrphaned})
	ps.Update("p", func(p *pipeline.Pipeline) {
		p.Job("a").Status = pipeline.JobFailed
		p.Job("a").SessionID = "p-a"
		p.Job("b").Status = pipeline.JobSkipped
		p.Status = pipeline.StatusStalled
	})

	if err := e.Retry(context.Background(), "p", "a"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("retried a should be running, got %s", got.Job("a").Status)
	}
	if got.Job("b").Status != pipeline.JobPending {
		t.Fatalf("skipped descendant b should be reopened to pending, got %s", got.Job("b").Status)
	}
	if got.Status != pipeline.StatusRunning {
		t.Fatalf("pipeline should be running again, got %s", got.Status)
	}
	// the stale session was deleted so the re-spawn could reuse the id.
	if _, err := ss.Get(context.Background(), "p-a"); err == nil {
		// a fresh p-a was inserted by the re-spawn; confirm it's the new running one.
		s, _ := ss.Get(context.Background(), "p-a")
		if s.Status == store.StatusOrphaned {
			t.Fatalf("stale orphaned session was not replaced")
		}
	}
}

func TestRetryRejectsNonRetryable(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	// a is pending (not failed/needs_attention) → not retryable.
	if err := e.Retry(context.Background(), "p", "a"); !errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("want ErrJobNotRetryable, got %v", err)
	}
	if err := e.Retry(context.Background(), "p", "ghost"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestOnTransitionIdleFlagsNeedsAttention(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // a running, session p-a inserted
	sess, _ := ss.Get(context.Background(), "p-a")

	e.OnTransition(sess, store.StatusWorking, store.StatusIdle)
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobNeedsAttention {
		t.Fatalf("idle running job should be needs_attention, got %s", got.Job("a").Status)
	}
	if got.Status != pipeline.StatusRunning {
		t.Fatalf("pipeline should stay running, got %s", got.Status)
	}
}

func TestEmitAllowedOnNeedsAttention(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")
	ps.Update("p", func(p *pipeline.Pipeline) { p.Job("a").Status = pipeline.JobNeedsAttention })

	if err := e.Emit(context.Background(), "p", "a", "finished after all"); err != nil {
		t.Fatalf("emit on needs_attention should be allowed: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobDone {
		t.Fatalf("emit should mark needs_attention job done, got %s", got.Job("a").Status)
	}
	if got.Job("b").Status != pipeline.JobRunning {
		t.Fatalf("dependent b should now run, got %s", got.Job("b").Status)
	}
}

func TestRetryReSkipsBranchBlockedByOtherFailure(t *testing.T) {
	// Two roots a, x; a->b, x->y. a failed (b skipped), x failed (y skipped).
	// Retrying a must reopen+run a/b but leave y skipped (still blocked by x).
	e, ps, _ := newTestExecutor(t)
	ps.Create(&pipeline.Pipeline{ID: "p2", Name: "p2", Repo: "/r", Status: pipeline.StatusStalled,
		Jobs: []pipeline.Job{
			{ID: "a", Prompt: "x", Worktree: "none", Status: pipeline.JobFailed},
			{ID: "b", Prompt: "x", DependsOn: []string{"a"}, Worktree: "none", Status: pipeline.JobSkipped},
			{ID: "x", Prompt: "x", Worktree: "none", Status: pipeline.JobFailed},
			{ID: "y", Prompt: "x", DependsOn: []string{"x"}, Worktree: "none", Status: pipeline.JobSkipped},
		}})
	if err := e.Retry(context.Background(), "p2", "a"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	got, _ := ps.Get("p2")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("a should be running, got %s", got.Job("a").Status)
	}
	if got.Job("y").Status != pipeline.JobSkipped {
		t.Fatalf("y must stay skipped (x still failed), got %s", got.Job("y").Status)
	}
	if got.Status != pipeline.StatusStalled {
		t.Fatalf("pipeline still has a failed job (x) → stalled, got %s", got.Status)
	}
}

func TestOnTransitionResumeClearsNeedsAttention(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")
	sess, _ := ss.Get(context.Background(), "p-a")
	e.OnTransition(sess, store.StatusWorking, store.StatusIdle) // → needs_attention
	e.OnTransition(sess, store.StatusIdle, store.StatusWorking) // resumed → running
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("resumed job should be running again, got %s", got.Job("a").Status)
	}
}

func TestExecutorJobDigestAccessor(t *testing.T) {
	ps, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("pipeline.NewStore: %v", err)
	}
	_ = ps.Create(&pipeline.Pipeline{
		ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Prompt: "x", Status: pipeline.JobDone,
			Digest: &digest.Digest{Summary: "snap"}}},
	})
	e := NewExecutor(ps, newFakeStore(), &fakeLife{}, nil, func() {})
	if got := e.JobDigest("p", "a"); got == nil || got.Summary != "snap" {
		t.Fatalf("want snap, got %+v", got)
	}
	if got := e.JobDigest("p", "missing"); got != nil {
		t.Fatalf("want nil for unknown job, got %+v", got)
	}
}
