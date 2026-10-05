package autopilot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// awaitingSetup drives a run to awaiting_merge on a controllable clock.
func awaitingSetup(t *testing.T) (*Controller, *completionRT, string, *time.Time) {
	t.Helper()
	c, rt, id := completionSetup(t, nil)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.completionTick(context.Background()) // opens the final PR
	rt.state.Gate = GateGreen
	c.completionTick(context.Background())
	require.Equal(t, StateAwaitingMerge, runState(c, id))
	return c, rt, id, &now
}

func nextPollTick(c *Controller, now *time.Time) {
	*now = now.Add(DefaultMergePollInterval + time.Second)
	c.completionTick(context.Background())
}

func TestAwaitingMergeEntryTearsDownAndNotifiesOnce(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	require.Equal(t, []string{id}, rt.reaped)
	c.mu.Lock()
	require.Nil(t, c.runs[id].brain, "manager torn down")
	c.mu.Unlock()
	st, err := c.LookupRun(id)
	require.NoError(t, err)
	require.NotNil(t, st.AwaitingMerge)
	require.Equal(t, 99, st.AwaitingMerge.PR)
	require.Equal(t, "check final PR #99 for merge", st.NextStep.Action)
	require.Equal(t, "daemon", st.NextStep.Owner)

	n := len(rt.escalations)
	nextPollTick(c, now)
	require.Len(t, rt.escalations, n, "polling never re-notifies")
	require.Equal(t, StateAwaitingMerge, runState(c, id))
}

func TestAwaitingMergePollIsRateLimited(t *testing.T) {
	c, rt, _, now := awaitingSetup(t)
	nextPollTick(c, now)
	v := rt.views
	*now = now.Add(time.Second)
	c.completionTick(context.Background())
	require.Equal(t, v, rt.views, "no second poll inside the interval")
}

func TestAwaitingMergeMergedCompletes(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	rt.state = FinalPRState{State: "merged", HeadSHA: "head1", MergedAt: "2026-10-05T13:00:00Z"}
	nextPollTick(c, now)
	require.Equal(t, StateComplete, runState(c, id))
	st, _ := c.LookupRun(id)
	require.Equal(t, "merged", st.FinalPR.State)
	require.Contains(t, rt.escalations[len(rt.escalations)-1], "merged")
	require.Zero(t, rt.mergedPRs)
}

func TestAwaitingMergeOpenCleanKeepsWaiting(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	rt.state.Mergeable, rt.state.MergeStateStatus = "MERGEABLE", "CLEAN"
	nextPollTick(c, now)
	require.Equal(t, StateAwaitingMerge, runState(c, id))
}

func TestAwaitingMergeHeadMovedRegates(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	rt.state.HeadSHA = "head2"
	nextPollTick(c, now)
	require.Equal(t, StateFinalizing, runState(c, id))
	rt.state.Gate = GateGreen
	n := len(rt.escalations)
	c.completionTick(context.Background())
	require.Equal(t, StateAwaitingMerge, runState(c, id), "re-gated green returns to awaiting")
	require.Len(t, rt.escalations, n, "no second notification")
}

func TestAwaitingMergeConflictingReentersBaseMerge(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	merges := rt.merges
	rt.state.Mergeable = "CONFLICTING"
	nextPollTick(c, now)
	require.Equal(t, StateFinalizing, runState(c, id))
	rt.state.Mergeable = "MERGEABLE"
	c.completionTick(context.Background())
	require.Greater(t, rt.merges, merges, "default branch merged again")
}

func TestAwaitingMergeBehindReentersCompletion(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	rt.state.MergeStateStatus = "BEHIND"
	nextPollTick(c, now)
	require.Equal(t, StateFinalizing, runState(c, id))
}

func TestAwaitingMergeClosedParks(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	rt.state = FinalPRState{State: "closed"}
	nextPollTick(c, now)
	c.mu.Lock()
	na := c.runs[id].needsAttention
	c.mu.Unlock()
	require.Contains(t, na, "final_pr_closed")
	require.Contains(t, na, "wd plan resume")
	require.NotEqual(t, StateAwaitingMerge, runState(c, id))
}

func TestAwaitingMergePollErrorIsRetried(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	rt.viewErr = context.DeadlineExceeded
	nextPollTick(c, now)
	require.Equal(t, StateAwaitingMerge, runState(c, id))
	rt.viewErr = nil
	rt.state = FinalPRState{State: "merged"}
	nextPollTick(c, now)
	require.Equal(t, StateComplete, runState(c, id))
}

func TestAwaitingMergeTasksAppendedLeavesAwaiting(t *testing.T) {
	c, _, id, now := awaitingSetup(t)
	c.mu.Lock()
	c.runs[id].plan.Tasks = append(c.runs[id].plan.Tasks, PlanTask{ID: "t2", Status: TaskStatusPending})
	c.mu.Unlock()
	nextPollTick(c, now)
	require.Equal(t, StateActive, runState(c, id))
}

func TestAwaitingMergeStalePollDiscarded(t *testing.T) {
	c, _, id, _ := awaitingSetup(t)
	c.mu.Lock()
	stale := *c.runs[id].completion.awaiting
	c.mu.Unlock()
	require.True(t, c.awaitingCurrent(id, &stale))
	c.leaveAwaiting(context.Background(), id, "test")
	require.False(t, c.awaitingCurrent(id, &stale), "left the episode")
	c.mu.Lock()
	c.runs[id].completion.awaitGen++
	c.runs[id].completion.awaiting = &AwaitingMerge{PR: stale.PR, gen: c.runs[id].completion.awaitGen}
	c.mu.Unlock()
	require.False(t, c.awaitingCurrent(id, &stale), "new episode, old generation")
}

func TestAwaitingMergeRestartResumesPollingWithoutManager(t *testing.T) {
	c, rt, id, now := awaitingSetup(t)
	// Simulate a daemon restart: in-memory completion state is lost, the
	// persisted surface record survives.
	c.mu.Lock()
	r := c.runs[id]
	r.completion = completionState{}
	r.surface, r.surfaceLoaded = surfaceRecord{}, false
	c.mu.Unlock()
	require.Equal(t, StateAwaitingMerge, runState(c, id))

	ensured, n := len(rt.ensured), len(rt.escalations)
	// Squash-merged while the daemon was down: no second PR is opened.
	rt.state = FinalPRState{State: "merged"}
	nextPollTick(c, now)
	require.Equal(t, StateComplete, runState(c, id))
	require.Len(t, rt.ensured, ensured, "no second final PR")
	require.Len(t, rt.escalations, n+1)
}

func TestFinalizingRestartReadsRecordedPRBeforeEnsure(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	c.completionTick(context.Background()) // opens PR #99, still pending
	c.mu.Lock()
	r := c.runs[id]
	r.completion = completionState{}
	r.surface, r.surfaceLoaded = surfaceRecord{}, false
	c.mu.Unlock()
	ensured := len(rt.ensured)
	rt.state = FinalPRState{State: "merged"}
	c.completionTick(context.Background())
	require.Equal(t, StateComplete, runState(c, id))
	require.Len(t, rt.ensured, ensured, "EnsureFinalPR must not run for a merged recorded PR")
}

func TestAwaitingMergeGuardianSkipsRun(t *testing.T) {
	c, rt, id, _ := awaitingSetup(t)
	c.guardianTick(context.Background())
	c.overwatchTick(context.Background())
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Nil(t, c.runs[id].brain, "no manager respawned for a waiting run")
	require.Equal(t, StateActive, c.runs[id].state)
	require.Empty(t, rt.nudges[len(rt.nudges):])
}

func TestMergePollIntervalFloor(t *testing.T) {
	require.Equal(t, DefaultMergePollInterval, CompletionPolicy{}.withDefaults().MergePollInterval)
	require.Equal(t, MinMergePollInterval, CompletionPolicy{MergePollInterval: time.Second}.withDefaults().MergePollInterval)
}
