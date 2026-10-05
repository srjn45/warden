package autopilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TeardownRunAgents makes guardianFake a RestartRuntime for RestartRun tests.
func (f *guardianFake) TeardownRunAgents(_ context.Context, runID, _, _ string) (RestartTeardown, error) {
	if f.teardownErr != nil {
		return RestartTeardown{}, f.teardownErr
	}
	td := f.teardownResult
	if td.AgentsRemoved == 0 && len(f.roster[runID]) > 0 {
		td.AgentsRemoved = len(f.roster[runID])
	}
	f.teardowns = append(f.teardowns, runID)
	delete(f.roster, runID)
	return td, nil
}

func restartHarness(t *testing.T) (*parkHarness, string) {
	t.Helper()
	h := newParkHarness(t)
	runID := h.r.runID
	// Seed a manager + worker so teardown has something to report.
	h.fake.roster[runID] = []AgentInfo{
		{ID: "mgr-1", Role: "autopilot"},
		{ID: "worker-1", Role: "worker"},
	}
	h.r.brain = &BrainHandle{AgentID: "mgr-1", Backend: "a"}
	h.r.state = StateActive
	ledger := h.fake.NewLedger(runID)
	require.NoError(t, ledger.WriteTasks([]LedgerTask{
		{ID: "t1", State: LedgerLanded, Branch: "w/t1", PR: 11, WorkerID: "old-1"},
		{ID: "t2", State: LedgerInProgress, Branch: "w/t2", PR: 12, WorkerID: "worker-1"},
		{ID: "t3", State: LedgerPending, WorkerID: "worker-gone"},
	}, "test"))
	require.NoError(t, ledger.AppendLanding(Landing{
		Branch: "w/t1", SHA: "abc123", PR: 11, LandedAt: "2026-10-05T01:00:00Z",
	}))
	h.fake.teardownResult = RestartTeardown{
		AgentsRemoved:   2,
		BranchesKept:    []string{"w/t2"},
		BranchesDeleted: []string{"w/empty"},
	}
	return h, runID
}

func TestRestartRunFromStopped(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateStopped
	h.r.brain = nil

	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.NoError(t, err)
	require.Equal(t, RestartReasonOperatorStop, res.ReasonKind)
	require.Equal(t, StateActive, res.Status.State)
	require.Equal(t, 2, res.AgentsRemoved)
	require.Equal(t, []string{"w/t2"}, res.BranchesKept)
	require.Equal(t, []string{"w/empty"}, res.BranchesDeleted)
	require.Equal(t, "a", res.Backend)
	require.Equal(t, 1, res.RestartCount)
	require.NotNil(t, h.c.runs[runID].brain)
	require.NotEqual(t, "mgr-1", h.c.runs[runID].brain.AgentID, "fresh manager, not the old one")
	require.Equal(t, []string{runID}, h.fake.teardowns)
	require.NotEmpty(t, h.fake.spawned)
	require.Contains(t, h.fake.spawned[len(h.fake.spawned)-1].Prompt, "Restart context",
		"new manager digest must include the captured restart context")
}

func TestRestartRunFromDegradedBackoff(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateDegraded
	h.r.brain = nil
	h.r.backoffLastErr = "spawn failed: no backends"
	h.r.backoffKind = KindNoBackendSelectable
	h.r.healStage = stageBackoff
	h.r.tried = map[string]bool{"a": true}

	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.NoError(t, err)
	require.Equal(t, RestartReasonDegradedBackoff, res.ReasonKind)
	require.Equal(t, StateActive, h.c.runs[runID].state)
	require.Equal(t, stageHealthy, h.c.runs[runID].healStage)
	require.Empty(t, h.c.runs[runID].tried)
	require.Empty(t, h.c.runs[runID].backoffLastErr)
}

func TestRestartRunFromParked(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateDegraded
	h.r.brain = nil
	h.r.needsAttention = "definition_error: plan not loadable"
	h.r.parkedPlanKey = "1:sha256:one"

	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.NoError(t, err)
	require.Equal(t, RestartReasonNeedsAttention, res.ReasonKind)
	require.Empty(t, h.c.runs[runID].needsAttention)
	require.Empty(t, h.c.runs[runID].parkedPlanKey)
	require.Equal(t, StateActive, h.c.runs[runID].state)
}

func TestRestartRunFromHealing(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateHealing
	h.r.healStage = stageNudged
	h.r.healNextAt = h.clock.t.Add(time.Hour)

	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.NoError(t, err)
	require.Equal(t, StateActive, res.Status.State)
	require.Equal(t, stageHealthy, h.c.runs[runID].healStage)
	require.True(t, h.c.runs[runID].healNextAt.IsZero())
}

func TestRestartRunRefusesActiveWithoutForce(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateActive

	_, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.ErrorIs(t, err, ErrRunConflict)
	require.Contains(t, err.Error(), "pass --force")
	require.Empty(t, h.fake.teardowns, "must not tear down when refused")
}

func TestRestartRunForceFromActive(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateActive
	oldBrain := h.r.brain.AgentID

	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{Force: true})
	require.NoError(t, err)
	require.Equal(t, RestartReasonOperatorForce, res.ReasonKind)
	require.Equal(t, StateActive, res.Status.State)
	require.NotEqual(t, oldBrain, h.c.runs[runID].brain.AgentID)
	require.Empty(t, h.fake.roster[runID], "old agents cleared by teardown")
}

func TestRestartRunPreservesLandedLedgerAndResetsUnfinished(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateStopped
	h.r.brain = nil

	_, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.NoError(t, err)

	tasks, err := h.fake.NewLedger(runID).Tasks()
	require.NoError(t, err)
	require.Len(t, tasks, 3)
	require.Equal(t, LedgerLanded, tasks[0].State)
	require.Equal(t, "old-1", tasks[0].WorkerID, "landed worker_id preserved")
	require.Equal(t, LedgerPending, tasks[1].State)
	require.Empty(t, tasks[1].WorkerID, "unfinished worker cleared")
	require.Equal(t, "w/t2", tasks[1].Branch, "branch kept for re-issue")
	require.Equal(t, 12, tasks[1].PR)
	require.Equal(t, LedgerPending, tasks[2].State)
	require.Empty(t, tasks[2].WorkerID)

	lands, err := h.fake.NewLedger(runID).Landings()
	require.NoError(t, err)
	require.Len(t, lands, 1)
	require.Equal(t, "w/t1", lands[0].Branch)
}

func TestRestartRunReissueAfterMidwayFailure(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateStopped
	h.r.brain = nil
	h.fake.teardownErr = errors.New("worktree busy")

	_, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "teardown")
	require.Equal(t, StateStopped, h.c.runs[runID].state)

	h.fake.teardownErr = nil
	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{})
	require.NoError(t, err)
	require.Equal(t, StateActive, res.Status.State)
	require.Len(t, h.fake.teardowns, 1, "failed attempt did not record teardown; re-issue ran once")
	require.GreaterOrEqual(t, res.RestartCount, 1)
}

func TestRestartRunBackendOverride(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateStopped
	h.r.brain = nil

	res, err := h.c.RestartRun(context.Background(), runID, RestartRequest{Backend: "cursor"})
	require.NoError(t, err)
	require.Equal(t, "cursor", res.Backend)
	require.Equal(t, "cursor", h.fake.spawned[len(h.fake.spawned)-1].Backend)
}

func TestRestartRunRefusesComplete(t *testing.T) {
	h, runID := restartHarness(t)
	h.r.state = StateComplete

	_, err := h.c.RestartRun(context.Background(), runID, RestartRequest{Force: true})
	require.ErrorIs(t, err, ErrRunConflict)
	require.Contains(t, err.Error(), "terminal")
}

func TestRestartRunNotFound(t *testing.T) {
	h, _ := restartHarness(t)
	_, err := h.c.RestartRun(context.Background(), "ap-missing", RestartRequest{})
	require.ErrorIs(t, err, ErrRunNotFound)
}

func TestRestartRunUnsupportedRuntime(t *testing.T) {
	// A bare fakeRuntime (no TeardownRunAgents) must refuse clearly.
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		Plans:    []string{plan},
		BaseDir:  dir,
		Resolver: cyclicResolver("a", "free"),
		Guardian: testGuardian(),
	}, &fakeEnv{})
	c.SetRuntime(rt)
	st, err := c.ReconcileConfiguredPlans(context.Background(), "")
	require.NoError(t, err)
	runID := st.Runs[0].RunID
	c.mu.Lock()
	c.runs[runID].state = StateStopped
	c.mu.Unlock()

	_, err = c.RestartRun(context.Background(), runID, RestartRequest{})
	require.ErrorIs(t, err, ErrRestartUnsupported)
}
