package autopilot

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// completionRT adds the CompletionRuntime + ResolverRuntime seams.
type completionRT struct {
	*landingRT
	merge     MergeDefaultResult
	mergeErr  error
	merges    int
	finalErr  error
	ensured   []FinalPRSpec
	state     FinalPRState
	resolved  []ResolverSpawn
	mergedPRs int // number of host Merge calls against the final PR (must stay 0)
	views     int // FinalPRView calls (the awaiting poll)
	viewErr   error
	reaped    []string
}

func (r *completionRT) FinalPRView(context.Context, string, FinalPR) (FinalPRState, error) {
	r.views++
	if r.viewErr != nil {
		return FinalPRState{}, r.viewErr
	}
	return r.state, nil
}
func (r *completionRT) TerminateRunAgents(_ context.Context, runID string) error {
	r.reaped = append(r.reaped, runID)
	return nil
}

func (r *completionRT) MergeDefault(context.Context, string, string, string) (MergeDefaultResult, error) {
	r.merges++
	return r.merge, r.mergeErr
}
func (r *completionRT) EnsureFinalPR(_ context.Context, _ string, s FinalPRSpec) (FinalPR, error) {
	if r.finalErr != nil {
		return FinalPR{}, r.finalErr
	}
	r.ensured = append(r.ensured, s)
	sha := r.state.HeadSHA
	if sha == "" {
		sha = "head1"
	}
	return FinalPR{Number: 99, URL: "https://x/pull/99", HeadSHA: sha}, nil
}
func (r *completionRT) FinalPRStatus(context.Context, string, string, FinalPR, string) (FinalPRState, error) {
	return r.state, nil
}
func (r *completionRT) SpawnResolver(_ context.Context, s ResolverSpawn) (string, error) {
	r.resolved = append(r.resolved, s)
	return "resolver-1", nil
}

func completionSetup(t *testing.T, doneWhen []string) (*Controller, *completionRT, string) {
	t.Helper()
	c, lrt, runID := landingSetup(t)
	rt := &completionRT{landingRT: lrt, merge: MergeUpToDate, state: FinalPRState{State: "open", HeadSHA: "head1", Gate: GatePending}}
	c.SetRuntime(rt)
	c.mu.Lock()
	r := c.runs[runID]
	r.plan.Tasks = []PlanTask{{ID: "t1", Status: TaskStatusDone, LandedPR: 1}}
	r.plan.DoneWhen = doneWhen
	r.plan.Goal = "ship it"
	c.mu.Unlock()
	return c, rt, runID
}

func runState(c *Controller, id string) RunState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reportedStateLocked(c.runs[id])
}

func TestCompletionWaitsForOpenRunPRs(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	addPR(rt.landingRT, 5, "w/a", "sha1", "MERGEABLE", GatePending, true)
	c.completionTick(context.Background())
	require.Empty(t, rt.ensured)
	require.Equal(t, StateActive, runState(c, id))

	// A foreign (unowned) PR never blocks completion.
	rt.host.prs, rt.owners = nil, map[string]LandOwner{}
	addPR(rt.landingRT, 6, "someone/else", "sha2", "MERGEABLE", GatePending, false)
	c.completionTick(context.Background())
	require.Len(t, rt.ensured, 1)
}

func TestCompletionOpensFinalPRAndStaysFinalizingUntilGreen(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	c.completionTick(context.Background())
	require.Equal(t, StateFinalizing, runState(c, id))
	require.Len(t, rt.ensured, 1)
	require.Equal(t, "main", rt.ensured[0].DefaultBranch)
	require.Contains(t, rt.ensured[0].Body, "Autopilot never merges this PR")
	require.Contains(t, rt.ensured[0].Body, "| t1 | #1 |")
	st, err := c.LookupRun(id)
	require.NoError(t, err)
	require.Equal(t, StateFinalizing, st.State)
	require.Equal(t, FinalGatePending, st.FinalPR.Gate)

	rt.state.Gate = GateGreen
	c.completionTick(context.Background())
	require.Equal(t, StateAwaitingMerge, runState(c, id), "a green final PR does not complete the run")
	require.Contains(t, rt.escalations[len(rt.escalations)-1], "final PR #99 is green")
	require.Equal(t, []string{id}, rt.reaped)
	require.Zero(t, rt.mergedPRs)
}

func TestCompletionHumanMergedFinalPRCompletesImmediately(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	rt.state = FinalPRState{State: "merged"}
	c.completionTick(context.Background())
	require.Equal(t, StateComplete, runState(c, id))
}

func TestCompletionNothingToMergeCompletes(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	rt.finalErr = ErrNothingToMerge
	c.completionTick(context.Background())
	require.Equal(t, StateComplete, runState(c, id))
}

func TestCompletionBaseMergeConflictCallsResolverOnce(t *testing.T) {
	c, rt, _ := completionSetup(t, nil)
	rt.merge = MergeConflict
	c.completionTick(context.Background())
	c.completionTick(context.Background()) // held off while the resolver works
	require.Len(t, rt.resolved, 1)
	require.Equal(t, BlockerBaseMerge, rt.resolved[0].Class)
	require.Equal(t, "autopilot/x", rt.resolved[0].Branch)
	require.Equal(t, 1, rt.merges)
	require.Empty(t, rt.ensured)
}

func TestCompletionSkipMergeDefault(t *testing.T) {
	c, rt, _ := completionSetup(t, nil)
	c.SetCompletionPolicy(CompletionPolicy{SkipMergeDefault: true})
	c.completionTick(context.Background())
	require.Zero(t, rt.merges)
	require.Len(t, rt.ensured, 1)
}

func TestCompletionDoneWhenManagerFirstThenResolver(t *testing.T) {
	c, rt, id := completionSetup(t, []string{"docs updated"})
	clock := c.now
	_ = clock
	c.completionTick(context.Background())
	require.Empty(t, rt.ensured, "manager is asked first")
	require.NotEmpty(t, rt.nudges)

	// Manager confirms → PR opens.
	_, err := c.MarkVerified(id)
	require.NoError(t, err)
	c.completionTick(context.Background())
	require.Len(t, rt.ensured, 1)
	require.Contains(t, rt.ensured[0].Body, "- [x] docs updated")
}

func TestCompletionDoneWhenTimeoutCallsResolver(t *testing.T) {
	c, rt, _ := completionSetup(t, []string{"docs updated"})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.completionTick(context.Background())
	now = now.Add(DefaultManagerVerifyTimeout + time.Minute)
	c.completionTick(context.Background())
	require.Len(t, rt.resolved, 1)
	require.Equal(t, BlockerDoneWhen, rt.resolved[0].Class)
	c.completionTick(context.Background())
	require.Len(t, rt.ensured, 1)
	require.Contains(t, rt.ensured[0].Body, "not confirmed by the manager")
}

func TestCompletionRedFinalPRFixesPerSHAThenParks(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	c.SetCompletionPolicy(CompletionPolicy{MaxFinalFixes: 2})
	rt.state = FinalPRState{State: "open", HeadSHA: "head1", Gate: GateRed, Detail: "ci failed"}
	c.completionTick(context.Background())
	c.completionTick(context.Background()) // same red head: no second dispatch
	require.Len(t, rt.resolved, 1)
	require.Equal(t, "autopilot/x", rt.resolved[0].BaseBranch)
	require.Contains(t, rt.resolved[0].Branch, "final-fix-1-")
	require.Equal(t, StateFinalizing, runState(c, id))

	rt.state.HeadSHA = "head2" // the head moved and is still red
	c.completionTick(context.Background())
	require.Len(t, rt.resolved, 2)
	require.Contains(t, rt.resolved[1].Branch, "final-fix-2-")
	require.Equal(t, StateFinalizing, runState(c, id))

	rt.state.HeadSHA = "head3" // third red head hits the bound
	c.completionTick(context.Background())
	c.mu.Lock()
	na := c.runs[id].needsAttention
	c.mu.Unlock()
	require.Contains(t, na, "final_pr_unfixable")
	require.Len(t, rt.resolved, 2) // no third dispatch
}

func TestCompletionRedFinalPRParksAfterBound(t *testing.T) {
	c, rt, id := completionSetup(t, nil)
	c.SetCompletionPolicy(CompletionPolicy{MaxFinalFixes: 1})
	rt.state = FinalPRState{State: "open", HeadSHA: "head1", Gate: GateRed, Detail: "ci failed"}
	c.completionTick(context.Background())
	require.Len(t, rt.resolved, 1)
	rt.state.HeadSHA = "head2"
	c.completionTick(context.Background())
	c.mu.Lock()
	na := c.runs[id].needsAttention
	c.mu.Unlock()
	require.Contains(t, na, "final_pr_unfixable")
	require.Contains(t, na, "autopilot will not merge the final PR")
}

func TestCompletionManagedOnlyWithRuntimeSeam(t *testing.T) {
	c, _, _ := landingSetup(t)
	require.False(t, c.CompletionManaged())
	c2, _, _ := completionSetup(t, nil)
	require.True(t, c2.CompletionManaged())
}

func TestCompletionNeverMergesFinalPR(t *testing.T) {
	// The landing host's Merge is the only merge path; the completion phase
	// never calls it for a PR into the default branch.
	c, rt, _ := completionSetup(t, nil)
	rt.state.Gate = GateGreen
	c.completionTick(context.Background())
	c.landingTick(context.Background())
	require.Empty(t, rt.host.merges)
}

func TestFinalPRBodyDeterministic(t *testing.T) {
	in := FinalPRBodyInput{
		RunID: "r1", Name: "demo", Goal: "ship it", Integration: "autopilot/x", DefaultBranch: "main",
		Tasks:    []PlanTask{{ID: "t1", Status: TaskStatusDone, LandedPR: 7}, {ID: "t2", Status: TaskStatusDone}},
		Landings: []Landing{{PR: 7, Branch: "w/t1", LandedAt: "2026-10-01T00:00:00Z"}, {PR: 8, Branch: "w/t2", LandedAt: "2026-10-02T00:00:00Z"}},
		DoneWhen: []string{"docs updated"},
		Verified: true, ResolverCalls: 2, FinalFixes: 1,
	}
	a, b := FinalPRBody(in), FinalPRBody(in)
	require.Equal(t, a, b)
	require.Contains(t, a, "## Autopilot run r1 — demo")
	require.Contains(t, a, "| t1 | #7 | w/t1 | 2026-10-01T00:00:00Z |")
	require.Contains(t, a, "- [x] docs updated — verified by manager")
	require.Contains(t, a, "Tasks landed: 2. Resolver calls: 2. Final-PR fixes: 1.")
	require.Contains(t, a, "Autopilot never merges this PR")

	in.Verified = false
	body := FinalPRBody(in)
	require.Contains(t, body, "- [ ] docs updated — not confirmed by the manager")
}
