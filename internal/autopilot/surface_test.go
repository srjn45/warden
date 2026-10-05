package autopilot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/fastbrain"
)

// restart simulates a daemon restart for the status surface: the in-memory
// record is dropped so the next status read must hydrate from the ledger.
func restart(c *Controller, runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.runs[runID]
	r.surface, r.surfaceLoaded, r.completion.finalPR = surfaceRecord{}, false, nil
}

func runStatusOf(t *testing.T, c *Controller, runID string) RunStatus {
	t.Helper()
	for _, r := range c.Status().Runs {
		if r.RunID == runID {
			return r
		}
	}
	t.Fatalf("run %s not in status", runID)
	return RunStatus{}
}

func TestSurfaceLastDiagnosisPersists(t *testing.T) {
	h := newTriageHarness(t, true, func(fastbrain.StallInput) fastbrain.StallDiagnosis {
		return fastbrain.StallDiagnosis{Action: fastbrain.ActionWait, Confidence: 0.9, Source: "model", Rationale: "long tool call"}
	})
	require.Nil(t, runStatusOf(t, h.c, h.runID).LastDiagnosis)
	h.stall()

	d := runStatusOf(t, h.c, h.runID).LastDiagnosis
	require.NotNil(t, d)
	require.Equal(t, "wait", d.Action)
	require.Equal(t, "applied", d.Outcome)
	require.Equal(t, "long tool call", d.Rationale)
	require.NotEmpty(t, d.At)

	restart(h.c, h.runID)
	d = runStatusOf(t, h.c, h.runID).LastDiagnosis
	require.NotNil(t, d, "diagnosis must survive a restart")
	require.Equal(t, "wait", d.Action)
}

func TestSurfaceDiagnosisFailOpenReason(t *testing.T) {
	h := newTriageHarness(t, true, func(fastbrain.StallInput) fastbrain.StallDiagnosis {
		return fastbrain.StallDiagnosis{Action: fastbrain.ActionMechanical, Source: "failopen", FailOpen: "timeout"}
	})
	h.stall()
	d := runStatusOf(t, h.c, h.runID).LastDiagnosis
	require.NotNil(t, d)
	require.Equal(t, "mechanical", d.Outcome)
	require.Equal(t, "timeout", d.FailOpenReason)
}

func TestSurfaceFixStateAndResolver(t *testing.T) {
	c, fr := fixSetup(t)
	var runID string
	c.mu.Lock()
	for id, r := range c.runs {
		runID = id
		r.plan.Tasks = []PlanTask{{ID: "t1"}, {ID: "t2"}}
	}
	c.mu.Unlock()

	require.Empty(t, runStatusOf(t, c, runID).Fix, "no fix state before any red PR")
	require.NoError(t, fr.NewLedger(runID).WriteFixState(FixState{Task: "t1", PR: 7, HeadSHA: "sha1", Kind: "red",
		RedStreak: 1, FixAttempts: 1, Fixing: true, LastDispatchedSHA: "sha1"}, "test"))

	st := runStatusOf(t, c, runID)
	require.Len(t, st.Fix, 1)
	require.Equal(t, FixStatus{Task: "t1", PR: 7, Gate: "red", Kind: "red", RedStreak: 1, FixAttempts: 1,
		Fixing: true, LastDispatchedSHA: "sha1", UpdatedAt: st.Fix[0].UpdatedAt}, st.Fix[0])

	started, err := c.SpawnResolver(context.Background(), ResolverRequest{RunID: runID, TaskID: "t1", Branch: "w/a", Class: BlockerRedGate, Detail: "x"})
	require.NoError(t, err)
	require.True(t, started)
	restart(c, runID)
	st = runStatusOf(t, c, runID)
	require.NotNil(t, st.Resolver)
	require.Equal(t, 1, st.Resolver.Attempts)
	require.Equal(t, "started", st.Resolver.LastOutcome)
	require.Equal(t, "t1", st.Resolver.LastTask)

	// Landing clears the red streak: the gate goes back to clear, counters stay.
	require.NoError(t, fr.NewLedger(runID).ClearFixState("t1", "test"))
	st = runStatusOf(t, c, runID)
	require.Equal(t, "clear", st.Fix[0].Gate)
	require.Equal(t, 1, st.Fix[0].FixAttempts)
}

func TestSurfaceResolverExhaustedOutcome(t *testing.T) {
	c, fr := fixSetup(t)
	var runID string
	c.mu.Lock()
	for id := range c.runs {
		runID = id
	}
	c.mu.Unlock()
	req := ResolverRequest{RunID: runID, TaskID: "t1", Branch: "w/a", Class: BlockerRedGate, Detail: "x"}
	for i := 0; i < MaxResolverAttempts; i++ {
		ok, err := c.SpawnResolver(context.Background(), req)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, _ := c.SpawnResolver(context.Background(), req)
	require.False(t, ok)
	rs := runStatusOf(t, c, runID).Resolver
	require.Equal(t, MaxResolverAttempts, rs.Attempts)
	require.Equal(t, "exhausted", rs.LastOutcome)
	_ = fr
}

func TestSurfaceFinalPRPersistsAcrossRestart(t *testing.T) {
	c, _ := fixSetup(t)
	var runID string
	c.mu.Lock()
	for id := range c.runs {
		runID = id
	}
	c.mu.Unlock()
	require.Nil(t, runStatusOf(t, c, runID).FinalPR)

	c.setFinalPR(runID, FinalPR{Number: 42, URL: "https://x/pull/42", HeadSHA: "abc"}, "red")
	st := runStatusOf(t, c, runID)
	require.Equal(t, &FinalPR{Number: 42, URL: "https://x/pull/42", HeadSHA: "abc", Gate: "red"}, st.FinalPR)

	restart(c, runID)
	st = runStatusOf(t, c, runID)
	require.NotNil(t, st.FinalPR, "final PR must survive a restart")
	require.Equal(t, 42, st.FinalPR.Number)
	require.Equal(t, "red", st.FinalPR.Gate)
}
