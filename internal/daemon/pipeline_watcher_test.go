package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// newWatcherFixture builds a PipelineWatcher backed by real in-memory stores.
func newWatcherFixture(t *testing.T, stuckRetryAfter, stallAfter time.Duration, autoRetry bool) (*PipelineWatcher, *Executor, *pipeline.Store, *fakeStore) {
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
	exec := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	w := NewPipelineWatcher(ps, ss, exec, stuckRetryAfter, stallAfter, autoRetry)
	return w, exec, ps, ss
}

// runningPipeline returns a single-job Pipeline in running state.
func runningPipeline(id, jobID string, jobStatus pipeline.JobStatus) *pipeline.Pipeline {
	return &pipeline.Pipeline{
		ID: id, Name: id, Repo: "/r",
		Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: jobID, Prompt: "do it", Worktree: "none", Status: jobStatus},
		},
	}
}

// seedSession inserts a minimal session so the orphan cross-check treats the
// job's agent as present.
func seedSession(ss *fakeStore, agentID, pid, jobID string) {
	_ = ss.Insert(context.Background(), &store.Session{
		ID:         agentID,
		PipelineID: pid,
		JobID:      jobID,
	})
}

// TestStartupReconcileCallsReconcileForRunningPipelines verifies that on the
// first tick the watcher reconciles every running pipeline, advancing any job
// that is ready to spawn.
func TestStartupReconcileCallsReconcileForRunningPipelines(t *testing.T) {
	w, _, ps, _ := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	// Pending root job inside a running pipeline — Reconcile should spawn it.
	p := runningPipeline("p1", "job-a", pipeline.JobPending)
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}

	w.tick(context.Background(), true /* firstTick */)

	got, _ := ps.Get("p1")
	if got.Job("job-a").Status != pipeline.JobRunning {
		t.Errorf("startup reconcile: want job-a running, got %s", got.Job("job-a").Status)
	}
}

// TestStuckJobBelowThresholdNoRetry verifies that a needs_attention job that
// has not been stuck for >= stuckRetryAfter is NOT retried.
func TestStuckJobBelowThresholdNoRetry(t *testing.T) {
	w, _, ps, ss := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	p := runningPipeline("p2", "job-b", pipeline.JobNeedsAttention)
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-b", "p2", "job-b")
	p.Jobs[0].SetAgentID("agent-b")
	_ = ps.Update("p2", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-b") })

	// First tick: records the "first seen" timestamp.
	w.tick(context.Background(), false)
	// Back-date to just under threshold.
	w.needsAttentionSince["p2/job-b"] = time.Now().Add(-9 * time.Minute)

	// Second tick: still below threshold — should NOT retry.
	w.tick(context.Background(), false)

	got, _ := ps.Get("p2")
	if got.Job("job-b").Status != pipeline.JobNeedsAttention {
		t.Errorf("below threshold: want needs_attention, got %s", got.Job("job-b").Status)
	}
}

// TestStuckJobAutoRetryOnFirstStrike verifies that a needs_attention job with
// AutoRetryCount==0 that has been stuck >= stuckRetryAfter IS auto-retried.
func TestStuckJobAutoRetryOnFirstStrike(t *testing.T) {
	w, _, ps, ss := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	p := runningPipeline("p3", "job-c", pipeline.JobNeedsAttention)
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-c", "p3", "job-c")
	_ = ps.Update("p3", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-c") })

	// Pretend the job has been stuck past the threshold.
	w.needsAttentionSince["p3/job-c"] = time.Now().Add(-11 * time.Minute)

	w.tick(context.Background(), false)

	got, _ := ps.Get("p3")
	j := got.Job("job-c")
	// Retry resets status to pending (then Reconcile may advance to running).
	if j.Status == pipeline.JobNeedsAttention {
		t.Errorf("auto-retry: want job-c NOT in needs_attention, still stuck")
	}
	if j.AutoRetryCount != 1 {
		t.Errorf("auto-retry: want AutoRetryCount==1, got %d", j.AutoRetryCount)
	}
}

// TestStuckJobNoRetryAfterOnce verifies that a job with AutoRetryCount==1 is
// NOT retried again, even when stuck past the threshold.
func TestStuckJobNoRetryAfterOnce(t *testing.T) {
	w, _, ps, ss := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	p := runningPipeline("p4", "job-d", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-d", "p4", "job-d")
	_ = ps.Update("p4", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-d") })

	// Past the threshold.
	w.needsAttentionSince["p4/job-d"] = time.Now().Add(-11 * time.Minute)

	w.tick(context.Background(), false)

	got, _ := ps.Get("p4")
	if got.Job("job-d").Status != pipeline.JobNeedsAttention {
		t.Errorf("second-strike guard: want needs_attention, got %s", got.Job("job-d").Status)
	}
}

// TestStallDetectionTriggersReconcile verifies that a pipeline whose job
// statuses have not changed for >= stallAfter receives a safety Reconcile.
func TestStallDetectionTriggersReconcile(t *testing.T) {
	w, _, ps, _ := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	p := runningPipeline("p5", "job-e", pipeline.JobPending)
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Prime the watcher so it sees no change and the last-change time is old.
	w.pipelineJobStates["p5"] = snapshotJobStatuses(p)
	w.pipelineLastChange["p5"] = time.Now().Add(-21 * time.Minute)

	w.tick(context.Background(), false)

	got, _ := ps.Get("p5")
	// The safety Reconcile should have spawned the pending job.
	if got.Job("job-e").Status != pipeline.JobRunning {
		t.Errorf("stall reconcile: want job-e running, got %s", got.Job("job-e").Status)
	}
}

// TestOrphanedJobMarkedFailed verifies that a running job whose agent session
// is absent from the store is marked failed and descendants are skipped.
func TestOrphanedJobMarkedFailed(t *testing.T) {
	w, _, ps, _ := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	p := &pipeline.Pipeline{
		ID: "p6", Name: "p6", Repo: "/r",
		Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "job-f", Prompt: "work", Worktree: "none", Status: pipeline.JobRunning},
			{ID: "job-g", Prompt: "next", Worktree: "none", DependsOn: []string{"job-f"}, Status: pipeline.JobPending},
		},
	}
	p.Jobs[0].SetAgentID("agent-orphan")
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Session "agent-orphan" is intentionally absent from the session store.

	w.tick(context.Background(), false)

	got, _ := ps.Get("p6")
	if got.Job("job-f").Status != pipeline.JobFailed {
		t.Errorf("orphan: want job-f failed, got %s", got.Job("job-f").Status)
	}
	if got.Job("job-g").Status != pipeline.JobSkipped {
		t.Errorf("orphan: want job-g skipped, got %s", got.Job("job-g").Status)
	}
}
