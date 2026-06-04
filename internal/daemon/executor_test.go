package daemon

import (
	"context"
	"sync"
	"testing"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
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
