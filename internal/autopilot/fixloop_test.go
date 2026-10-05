package autopilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixRT adds the FixRuntime seam to the landing fake.
type fixRT struct {
	*landingRT
	ev       FixEvidence
	flaky    bool
	live     FixLiveness
	spawnErr error
	reruns   [][]string
	sent     []string
	spawned  []FixSpawn
	audits   []string
	resolved []ResolverSpawn
}

func (f *fixRT) CIEvidence(context.Context, string, string, string, string) FixEvidence { return f.ev }
func (f *fixRT) ClassifyCI(context.Context, string, string, string) (bool, float64, string) {
	return f.flaky, 0.9, "model"
}
func (f *fixRT) RerunFailed(_ context.Context, _, _ string, ids []string) error {
	f.reruns = append(f.reruns, ids)
	return nil
}
func (f *fixRT) WorkerLiveness(context.Context, string) FixLiveness { return f.live }
func (f *fixRT) SendFix(_ context.Context, id, msg string) error {
	f.sent = append(f.sent, id+": "+msg)
	return nil
}
func (f *fixRT) SpawnFixWorker(_ context.Context, s FixSpawn) (string, error) {
	if f.spawnErr != nil {
		return "", f.spawnErr
	}
	f.spawned = append(f.spawned, s)
	return "fixup-1", nil
}
func (f *fixRT) AuditRunEvent(_ context.Context, _, action, _, _ string) {
	f.audits = append(f.audits, action)
}
func (f *fixRT) SpawnResolver(_ context.Context, s ResolverSpawn) (string, error) {
	f.resolved = append(f.resolved, s)
	return "resolver-1", nil
}

func fixSetup(t *testing.T) (*Controller, *fixRT) {
	c, rt, _ := landingSetup(t)
	fr := &fixRT{landingRT: rt, live: FixWorkerIdle,
		ev: FixEvidence{Jobs: []string{"ci"}, RunIDs: []string{"11"}, Log: "--- FAIL: TestX"}}
	c.SetRuntime(fr)
	return c, fr
}

func redFix(sha string, owner LandOwner) LandFix {
	return LandFix{PR: OpenPR{Number: 7, HeadRef: "w/a", HeadSHA: sha}, TaskID: "t1", Kind: FixRed, Detail: "CI failed", Owner: owner}
}

func runFix(c *Controller, fr *fixRT, f LandFix) {
	rt := fr.landingRT
	runID := "run"
	c.mu.Lock()
	for id := range c.runs {
		runID = id // the resolver seam looks the real run up
	}
	c.mu.Unlock()
	c.runFixLoop(context.Background(), fr, rt.NewLedger("run"), landSnapshot{runID: runID, repo: "/repo"}, f)
}

func TestFixRealFailureSendsLiveOwnerOncePerSHA(t *testing.T) {
	c, fr := fixSetup(t)
	f := redFix("sha1", LandOwner{TaskID: "t1", WorkerID: "w1"})
	runFix(c, fr, f)
	runFix(c, fr, f) // same SHA: no second dispatch
	require.Len(t, fr.sent, 1)
	require.Contains(t, fr.sent[0], "wd job done")
	require.Contains(t, fr.sent[0], "--- FAIL: TestX")
	require.Empty(t, fr.spawned)
	require.Equal(t, []string{"autopilot.ci_fix_dispatched"}, fr.audits)

	st, _ := fr.NewLedger("run").FixState("t1")
	require.Equal(t, 1, st.FixAttempts)
	require.True(t, st.Fixing)

	runFix(c, fr, redFix("sha2", LandOwner{TaskID: "t1", WorkerID: "w1"})) // new red SHA
	require.Len(t, fr.sent, 2)
	st, _ = fr.NewLedger("run").FixState("t1")
	require.Equal(t, 2, st.RedStreak)
}

func TestFixDeadWorkerGetsFixupWorker(t *testing.T) {
	c, fr := fixSetup(t)
	fr.live = FixWorkerGone
	runFix(c, fr, redFix("sha1", LandOwner{TaskID: "t1", WorkerID: "w1", Worktree: "/wt"}))
	require.Empty(t, fr.sent)
	require.Len(t, fr.spawned, 1)
	require.Equal(t, "w/a", fr.spawned[0].Branch)
	require.Equal(t, "/wt", fr.spawned[0].Worktree)
	require.Contains(t, fr.spawned[0].Prompt, "--- FAIL: TestX")

	// No owner at all → fix-up worker too.
	runFix(c, fr, redFix("sha2", LandOwner{TaskID: "t1"}))
	require.Len(t, fr.spawned, 2)
}

func TestFixSpawnFailureRetriesNextTick(t *testing.T) {
	c, fr := fixSetup(t)
	fr.live, fr.spawnErr = FixWorkerGone, errors.New("boom")
	f := redFix("sha1", LandOwner{TaskID: "t1"})
	runFix(c, fr, f)
	fr.spawnErr = nil
	runFix(c, fr, f)
	require.Len(t, fr.spawned, 1)
}

func TestFixFlakyRerunsOncePerSHAThenDispatches(t *testing.T) {
	c, fr := fixSetup(t)
	fr.flaky = true
	f := redFix("sha1", LandOwner{TaskID: "t1", WorkerID: "w1"})
	runFix(c, fr, f)
	require.Equal(t, [][]string{{"11"}}, fr.reruns)
	require.Empty(t, fr.sent, "flaky: retried instead of dispatched")
	require.Equal(t, []string{"autopilot.ci_rerun"}, fr.audits)

	runFix(c, fr, f) // still red at the same SHA after the rerun → real dispatch
	require.Len(t, fr.reruns, 1)
	require.Len(t, fr.sent, 1)
}

func TestFixRerunCapPerTask(t *testing.T) {
	c, fr := fixSetup(t)
	c.SetFixPolicy(FixPolicy{MaxReruns: 2})
	fr.flaky = true
	for _, sha := range []string{"s1", "s2", "s3"} {
		runFix(c, fr, redFix(sha, LandOwner{TaskID: "t1", WorkerID: "w1"}))
	}
	require.Len(t, fr.reruns, 2)
	require.Len(t, fr.sent, 1, "third SHA exceeds the rerun cap and dispatches")
}

func TestFixAttemptCapStopsDispatch(t *testing.T) {
	c, fr := fixSetup(t)
	c.SetFixPolicy(FixPolicy{MaxFixes: 2, MaxRedSHAs: 99})
	for _, sha := range []string{"s1", "s2", "s3", "s4"} {
		runFix(c, fr, redFix(sha, LandOwner{TaskID: "t1", WorkerID: "w1"}))
	}
	require.Len(t, fr.sent, 2)
	require.Contains(t, fr.audits, "autopilot.ci_fix_capped")
	require.Len(t, fr.resolved, 2, "capped SHAs offer the resolver seam once each")
}

func TestFixRedStreakCallsResolverSeam(t *testing.T) {
	c, fr := fixSetup(t)
	c.SetFixPolicy(FixPolicy{MaxRedSHAs: 2})
	runFix(c, fr, redFix("s1", LandOwner{TaskID: "t1", WorkerID: "w1"}))
	require.Empty(t, fr.resolved)
	runFix(c, fr, redFix("s2", LandOwner{TaskID: "t1", WorkerID: "w1"}))
	require.Len(t, fr.resolved, 1)
	require.Len(t, fr.sent, 1, "resolver spawn does not stop dispatch of the first fix")
}

func TestFixBusyOwnerDefersThenWakes(t *testing.T) {
	c, fr := fixSetup(t)
	fr.live = FixWorkerBusy
	clock := &fakeClock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	c.setClock(clock.now)
	f := redFix("sha1", LandOwner{TaskID: "t1", WorkerID: "w1"})
	runFix(c, fr, f)
	require.Empty(t, fr.sent)
	clock.t = clock.t.Add(DefaultFixBusyDefer + time.Minute)
	runFix(c, fr, f)
	require.Len(t, fr.sent, 1)
}

func TestFixConflictDispatch(t *testing.T) {
	c, fr := fixSetup(t)
	f := redFix("sha1", LandOwner{TaskID: "t1", WorkerID: "w1"})
	f.Kind = FixConflict
	runFix(c, fr, f)
	require.Contains(t, fr.sent[0], "conflict")
	require.Empty(t, fr.reruns)
}

func TestFixLandingTickRoutesRedToFixLoop(t *testing.T) {
	c, fr := fixSetup(t)
	addPR(fr.landingRT, 1, "w/a", "sha1", "MERGEABLE", GateRed, true)
	fr.owners["w/a"] = LandOwner{TaskID: "t1", WorkerID: "w1", Worktree: "wt-w/a"}
	c.landingTick(context.Background())
	c.landingTick(context.Background())
	require.Empty(t, fr.host.merges)
	require.Empty(t, fr.fixes, "FixRuntime handles it, not the stub hook")
	require.Len(t, fr.sent, 1)
}
