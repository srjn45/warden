package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/pipeline"
)

// mockConsultor is a test double for brainconsult.Consultor.
type mockConsultor struct {
	called atomic.Int32
	result brainconsult.Result
	err    error
	// capturedReq is the most recent Request passed to Consult.
	capturedReq brainconsult.Request
}

func (m *mockConsultor) Consult(_ context.Context, req brainconsult.Request) (brainconsult.Result, error) {
	m.called.Add(1)
	m.capturedReq = req
	return m.result, m.err
}

// newBrainWatcherFixtureWithStores builds a PipelineWatcher with the brain consult
// feature wired in (mockConsultor, maxCon=1, fakeLife), returning all stores for
// direct manipulation.
func newBrainWatcherFixtureWithStores(t *testing.T, mc *mockConsultor) (*PipelineWatcher, *fakeLife, *pipeline.Store, *fakeStore) {
	t.Helper()
	w, _, ps, ss := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)
	fl := &fakeLife{}
	w.SetBrainConsult(mc, 1, fl)
	return w, fl, ps, ss
}

// waitForConsult polls until the mock consultor has been called at least once or
// the deadline elapses.
func waitForConsult(t *testing.T, mc *mockConsultor, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mc.called.Load() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("brain consult was not called within %s", timeout)
}

// waitForConsultN polls until the mock consultor has been called at least n times.
func waitForConsultN(t *testing.T, mc *mockConsultor, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if int(mc.called.Load()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("brain consult not called %d time(s) within %s (got %d)", n, timeout, mc.called.Load())
}

// TestBrainConsultNotFiresBeforeDeterministicRetry verifies that the brain
// consult does NOT fire when AutoRetryCount == 0 (the deterministic one-shot
// retry path runs first).
func TestBrainConsultNotFiresBeforeDeterministicRetry(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc1", "job-a", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 0 // first stuck episode → deterministic retry fires
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-a", "bc1", "job-a")
	_ = ps.Update("bc1", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-a") })

	// Back-date so the job is past stuckRetryAfter.
	w.needsAttentionSince["bc1/job-a"] = time.Now().Add(-11 * time.Minute)

	w.tick(context.Background(), false)

	// Give any spurious goroutine time to fire.
	time.Sleep(50 * time.Millisecond)

	if mc.called.Load() != 0 {
		t.Errorf("brain consult must NOT fire before deterministic retry: called %d time(s)", mc.called.Load())
	}
}

// TestBrainConsultFiresAfterDeterministicRetry verifies that the brain consult
// DOES fire when AutoRetryCount >= 1 (deterministic retry already exhausted).
func TestBrainConsultFiresAfterDeterministicRetry(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc2", "job-b", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1 // deterministic retry already fired
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-b", "bc2", "job-b")
	_ = ps.Update("bc2", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-b") })

	w.needsAttentionSince["bc2/job-b"] = time.Now().Add(-11 * time.Minute)

	w.tick(context.Background(), false)

	waitForConsult(t, mc, 2*time.Second)
}

// TestBrainConsultDedupePerEpisode verifies that the brain consult fires at most
// once per stuck episode: a second tick for the same job in the same episode
// must NOT trigger a second consult.
func TestBrainConsultDedupePerEpisode(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc3", "job-c", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-c", "bc3", "job-c")
	_ = ps.Update("bc3", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-c") })

	w.needsAttentionSince["bc3/job-c"] = time.Now().Add(-11 * time.Minute)

	// First tick: fires consult.
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)

	// Reload pipeline; job still in needs_attention (noop action didn't change it).
	// Second tick: must NOT fire another consult.
	w.tick(context.Background(), false)
	time.Sleep(50 * time.Millisecond)

	if mc.called.Load() != 1 {
		t.Errorf("consult must fire exactly once per episode, got %d call(s)", mc.called.Load())
	}
}

// TestBrainConsultFreshEpisodeAfterRetry verifies that after the brain fires
// retry_job (incrementing AutoRetryCount) and the job gets stuck again, a fresh
// consult is allowed for the new episode.
func TestBrainConsultFreshEpisodeAfterRetry(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionRetryJob}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc4", "job-d", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-d", "bc4", "job-d")
	_ = ps.Update("bc4", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-d") })

	w.needsAttentionSince["bc4/job-d"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)

	// Brain fired retry_job → AutoRetryCount is now 2; job went to pending.
	// Simulate the job becoming stuck again.
	_ = ps.Update("bc4", func(up *pipeline.Pipeline) {
		j := up.Job("job-d")
		if j != nil {
			j.Status = pipeline.JobNeedsAttention
		}
	})
	w.needsAttentionSince["bc4/job-d"] = time.Now().Add(-11 * time.Minute)

	// Switch to noop so we don't loop.
	mc.result = brainconsult.Result{Action: brainconsult.ActionNoop}

	w.tick(context.Background(), false)
	waitForConsultN(t, mc, 2, 2*time.Second)
}

// TestBrainConsultActionRetryJob verifies that the retry_job action calls
// Executor.Retry on the correct pipeline/job.
func TestBrainConsultActionRetryJob(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionRetryJob}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc5", "job-e", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-e", "bc5", "job-e")
	_ = ps.Update("bc5", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-e") })

	w.needsAttentionSince["bc5/job-e"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)

	// Give the goroutine time to execute the action.
	time.Sleep(100 * time.Millisecond)

	got, _ := ps.Get("bc5")
	j := got.Job("job-e")
	if j.Status == pipeline.JobNeedsAttention {
		t.Errorf("retry_job action: job should no longer be needs_attention")
	}
	if j.AutoRetryCount != 2 {
		t.Errorf("retry_job action: expected AutoRetryCount 2, got %d", j.AutoRetryCount)
	}
}

// TestBrainConsultActionMarkFailed verifies that the mark_failed action marks
// the job as JobFailed and triggers Reconcile (advancing dependents).
func TestBrainConsultActionMarkFailed(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionMarkFailed}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := &pipeline.Pipeline{
		ID: "bc6", Name: "bc6", Repo: "/r",
		Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "job-f", Prompt: "work", Worktree: "none", Status: pipeline.JobNeedsAttention, AutoRetryCount: 1},
			{ID: "job-g", Prompt: "next", Worktree: "none", DependsOn: []string{"job-f"}, Status: pipeline.JobPending},
		},
	}
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-f", "bc6", "job-f")
	_ = ps.Update("bc6", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-f") })

	w.needsAttentionSince["bc6/job-f"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)
	time.Sleep(100 * time.Millisecond)

	got, _ := ps.Get("bc6")
	if got.Job("job-f").Status != pipeline.JobFailed {
		t.Errorf("mark_failed action: expected job-f failed, got %s", got.Job("job-f").Status)
	}
	// Reconcile should have skipped the dependent.
	if got.Job("job-g").Status != pipeline.JobSkipped {
		t.Errorf("mark_failed action: expected job-g skipped (after reconcile), got %s", got.Job("job-g").Status)
	}
}

// TestBrainConsultActionSkipJob verifies that the skip_job action marks the job
// as JobSkipped and triggers Reconcile.
func TestBrainConsultActionSkipJob(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionSkipJob}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc7", "job-h", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-h", "bc7", "job-h")
	_ = ps.Update("bc7", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-h") })

	w.needsAttentionSince["bc7/job-h"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)
	time.Sleep(100 * time.Millisecond)

	got, _ := ps.Get("bc7")
	if got.Job("job-h").Status != pipeline.JobSkipped {
		t.Errorf("skip_job action: expected job-h skipped, got %s", got.Job("job-h").Status)
	}
}

// TestBrainConsultActionNudgeAgent verifies that the nudge_agent action calls
// life.Input with "continue" on the job's agent pane.
func TestBrainConsultActionNudgeAgent(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNudgeAgent}}
	w, fl, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc8", "job-i", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-i", "bc8", "job-i")
	_ = ps.Update("bc8", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-i") })

	w.needsAttentionSince["bc8/job-i"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)
	time.Sleep(100 * time.Millisecond)

	fl.mu.Lock()
	lastInput := fl.lastInput
	fl.mu.Unlock()
	if lastInput != "continue" {
		t.Errorf("nudge_agent action: expected life.Input called with 'continue', got %q", lastInput)
	}
}

// TestBrainConsultActionWaitNoop verifies that wait and noop actions leave the
// pipeline unchanged (no executor calls made that alter job status).
func TestBrainConsultActionWaitNoop(t *testing.T) {
	for _, action := range []brainconsult.Action{brainconsult.ActionWait, brainconsult.ActionNoop} {
		t.Run(string(action), func(t *testing.T) {
			mc := &mockConsultor{result: brainconsult.Result{Action: action}}
			w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

			pid := "bc9-" + string(action)
			p := runningPipeline(pid, "job-j", pipeline.JobNeedsAttention)
			p.Jobs[0].AutoRetryCount = 1
			if err := ps.Create(p); err != nil {
				t.Fatalf("create: %v", err)
			}
			seedSession(ss, "agent-j-"+string(action), pid, "job-j")
			_ = ps.Update(pid, func(up *pipeline.Pipeline) {
				up.Jobs[0].SetAgentID("agent-j-" + string(action))
			})

			w.needsAttentionSince[pid+"/job-j"] = time.Now().Add(-11 * time.Minute)
			w.tick(context.Background(), false)
			waitForConsult(t, mc, 2*time.Second)
			time.Sleep(50 * time.Millisecond)

			got, _ := ps.Get(pid)
			if got.Job("job-j").Status != pipeline.JobNeedsAttention {
				t.Errorf("action %s: expected job-j still needs_attention, got %s",
					action, got.Job("job-j").Status)
			}
		})
	}
}

// TestBrainConsultNotFiredWhenNilConsultor verifies that setting no consultor
// keeps the old "log and stop" behaviour.
func TestBrainConsultNotFiredWhenNilConsultor(t *testing.T) {
	// Use the base fixture — no SetBrainConsult call.
	w, _, ps, ss := newWatcherFixture(t, 10*time.Minute, 20*time.Minute, true)

	p := runningPipeline("bc10", "job-k", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 1
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-k", "bc10", "job-k")
	_ = ps.Update("bc10", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-k") })

	w.needsAttentionSince["bc10/job-k"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	time.Sleep(50 * time.Millisecond)

	// Job should remain needs_attention (no retry, no structural change).
	got, _ := ps.Get("bc10")
	if got.Job("job-k").Status != pipeline.JobNeedsAttention {
		t.Errorf("nil consultor: expected job-k still needs_attention, got %s", got.Job("job-k").Status)
	}
}

// TestBrainConsultRequestContent verifies that the Request passed to the
// Consultor contains the expected pipeline/job context.
func TestBrainConsultRequestContent(t *testing.T) {
	mc := &mockConsultor{result: brainconsult.Result{Action: brainconsult.ActionNoop}}
	w, _, ps, ss := newBrainWatcherFixtureWithStores(t, mc)

	p := runningPipeline("bc11", "job-l", pipeline.JobNeedsAttention)
	p.Jobs[0].AutoRetryCount = 2
	if err := ps.Create(p); err != nil {
		t.Fatalf("create: %v", err)
	}
	seedSession(ss, "agent-l", "bc11", "job-l")
	_ = ps.Update("bc11", func(up *pipeline.Pipeline) { up.Jobs[0].SetAgentID("agent-l") })

	w.needsAttentionSince["bc11/job-l"] = time.Now().Add(-11 * time.Minute)
	w.tick(context.Background(), false)
	waitForConsult(t, mc, 2*time.Second)

	req := mc.capturedReq
	if req.PipelineID != "bc11" {
		t.Errorf("expected PipelineID bc11, got %q", req.PipelineID)
	}
	if req.JobID != "job-l" {
		t.Errorf("expected JobID job-l, got %q", req.JobID)
	}
	if req.Intent == "" {
		t.Errorf("expected non-empty Intent")
	}
}
