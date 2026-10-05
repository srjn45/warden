package autopilot

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/stretchr/testify/require"
)

type owTriage struct {
	t     *testing.T
	c     *Controller
	fake  *guardianFake
	clock *fakeClock
	runID string
	mgr   string
	t0    time.Time
	calls atomic.Int32
	ins   chan fastbrain.StallInput
}

func newOWTriage(t *testing.T, useFB bool, diag func(fastbrain.StallInput) fastbrain.StallDiagnosis) *owTriage {
	t.Helper()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	g := testGuardian()
	g.UseFastBrain = useFB
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), g)
	h := &owTriage{t: t, c: c, fake: fake, clock: clock, runID: runID, t0: t0, mgr: c.runs[runID].brain.AgentID, ins: make(chan fastbrain.StallInput, 16)}
	c.triageFn = func(_ context.Context, in fastbrain.StallInput) fastbrain.StallDiagnosis {
		h.calls.Add(1)
		h.ins <- in
		return diag(in)
	}
	fake.roster[runID] = []AgentInfo{manager(h.mgr, "idle"), {ID: "w1", Name: "api", State: "waiting_for_input"}}
	return h
}

func (h *owTriage) tick(d time.Duration) {
	h.clock.add(d)
	h.c.overwatchTick(context.Background())
}

func (h *owTriage) settle() {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		h.c.mu.Lock()
		defer h.c.mu.Unlock()
		return h.c.runs[h.runID].wtriage.result != nil
	}, 2*time.Second, time.Millisecond)
}

// drive runs launch -> deliver -> apply and returns after the applying tick.
func (h *owTriage) drive() {
	h.tick(overwatchMinGap + time.Minute) // launch; nudge held while pending
	h.settle()
	h.tick(time.Second) // apply
}

func owModel(a fastbrain.StallAction, text string) func(fastbrain.StallInput) fastbrain.StallDiagnosis {
	return func(fastbrain.StallInput) fastbrain.StallDiagnosis {
		return fastbrain.StallDiagnosis{Action: a, Text: text, Confidence: 0.9, Source: "model", Rationale: "r"}
	}
}

func TestWorkerTriageResolvePromptSkipsManagerNudge(t *testing.T) {
	h := newOWTriage(t, true, owModel(fastbrain.ActionResolvePrompt, ""))
	h.drive()
	require.Equal(t, []string{"resolve:w1"}, h.fake.evCalls)
	require.Empty(t, h.fake.wakes, "a worker resolved directly is not reported to the manager")
	in := <-h.ins
	require.True(t, in.Worker, "worker action set")
}

func TestWorkerTriageResumeRateLimitAndRedeliver(t *testing.T) {
	for a, want := range map[fastbrain.StallAction]string{
		fastbrain.ActionResumeRateLimit: "resume:w1",
		fastbrain.ActionRedeliverPrompt: "redeliver:w1",
	} {
		h := newOWTriage(t, true, owModel(a, ""))
		h.drive()
		require.Equal(t, []string{want}, h.fake.evCalls, a)
		require.Empty(t, h.fake.wakes, a)
	}
}

func TestWorkerTriageNudgeBecomesSpecificFinding(t *testing.T) {
	h := newOWTriage(t, true, owModel(fastbrain.ActionNudge, "pushed but never ran wd job done"))
	h.drive()
	require.Empty(t, h.fake.evCalls)
	require.Len(t, h.fake.wakes, 1)
	require.Contains(t, h.fake.wakes[0], "api (w1, waiting_for_input): pushed but never ran wd job done")
}

func TestWorkerTriageWaitIsBounded(t *testing.T) {
	h := newOWTriage(t, true, owModel(fastbrain.ActionWait, ""))
	max := h.c.guardian.MaxWaits
	for i := 0; i < max; i++ {
		h.drive()
		require.Empty(t, h.fake.wakes, "wait %d defers the nudge", i)
	}
	h.drive()
	require.Len(t, h.fake.wakes, 1, "past the wait bound the generic nudge runs")
	require.NotContains(t, h.fake.wakes[0], ": pushed")
}

func TestWorkerTriageFailOpenKeepsGenericNudge(t *testing.T) {
	h := newOWTriage(t, true, func(fastbrain.StallInput) fastbrain.StallDiagnosis {
		return fastbrain.StallDiagnosis{Action: fastbrain.ActionMechanical, Source: "failopen", FailOpen: fastbrain.FailOpenTimeout}
	})
	h.drive()
	require.Len(t, h.fake.wakes, 1)
	require.Equal(t, overwatchNudgePrefix+"these workers are idle or waiting on input and need you — api (w1, waiting_for_input)"+
		". Check every worker that is waiting_for_input and answer or steer it; "+
		"clean up finished or idle workers (terminate then remove_worktree, mark the task landed in the ledger); "+
		"then pull the next pending task.", h.fake.wakes[0][len(h.mgr)+2:])
}

func TestWorkerTriageActionErrorFallsBackToNudge(t *testing.T) {
	h := newOWTriage(t, true, owModel(fastbrain.ActionResolvePrompt, ""))
	h.fake.evActErr = context.DeadlineExceeded
	h.drive()
	require.Len(t, h.fake.wakes, 1, "a failed direct action leaves the worker in the manager nudge")
}

func TestWorkerTriageOffParity(t *testing.T) {
	h := newOWTriage(t, false, owModel(fastbrain.ActionResolvePrompt, ""))
	h.tick(overwatchMinGap + time.Minute)
	require.EqualValues(t, 0, h.calls.Load(), "no model call with triage off")
	require.Len(t, h.fake.wakes, 1, "the nudge fires immediately, exactly as before")
	require.Empty(t, h.fake.evCalls)
}

func TestWorkerTriageCallsBoundedPerGap(t *testing.T) {
	h := newOWTriage(t, true, owModel(fastbrain.ActionNudge, "x"))
	roster := []AgentInfo{manager(h.mgr, "working")}
	for i := 0; i < 6; i++ {
		roster = append(roster, AgentInfo{ID: "w" + string(rune('1'+i)), State: "idle"})
	}
	h.fake.roster[h.runID] = roster
	h.tick(overwatchMinGap + time.Minute)
	h.settle()
	require.EqualValues(t, overwatchTriageMax, h.calls.Load(), "at most overwatchTriageMax diagnoses per pass")
	h.tick(time.Minute) // applies; still inside the gap -> no relaunch
	h.tick(time.Minute)
	require.EqualValues(t, overwatchTriageMax, h.calls.Load(), "no further calls inside the gap")
	h.tick(overwatchMinGap)
	h.settle()
	require.EqualValues(t, 2*overwatchTriageMax, h.calls.Load(), "next pass after the gap")
}

func TestWorkerTriageDiscardsStaleResult(t *testing.T) {
	h := newOWTriage(t, true, owModel(fastbrain.ActionResolvePrompt, ""))
	h.tick(overwatchMinGap + time.Minute)
	h.settle()
	h.fake.roster[h.runID] = []AgentInfo{manager(h.mgr, "idle"), {ID: "w1", Name: "api", State: "working"}}
	h.tick(time.Second)
	require.Empty(t, h.fake.evCalls, "a worker that went busy is not acted on")
}
