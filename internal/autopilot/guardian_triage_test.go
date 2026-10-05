package autopilot

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/stretchr/testify/require"
)

func (f *guardianFake) AgentEvidence(_ context.Context, _ string) (AgentEvidence, error) {
	return f.evidence, f.evErr
}
func (f *guardianFake) ResolvePrompt(_ context.Context, id string) error {
	f.evCalls = append(f.evCalls, "resolve:"+id)
	return f.evActErr
}
func (f *guardianFake) ResumeRateLimit(_ context.Context, id string) error {
	f.evCalls = append(f.evCalls, "resume:"+id)
	return f.evActErr
}
func (f *guardianFake) RedeliverPrompt(_ context.Context, id string) error {
	f.evCalls = append(f.evCalls, "redeliver:"+id)
	return f.evActErr
}

type triageHarness struct {
	t     *testing.T
	c     *Controller
	fake  *guardianFake
	clock *fakeClock
	runID string
	t0    time.Time
	calls atomic.Int32
}

// newTriageHarness builds a triage-enabled controller whose diagnosis is diag().
func newTriageHarness(t *testing.T, useFB bool, diag func(fastbrain.StallInput) fastbrain.StallDiagnosis) *triageHarness {
	t.Helper()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	res := &roundRobinResolver{backends: []string{"a", "b"}, tiers: []backendstore.ModelTier{"free", "subscription"}}
	g := testGuardian()
	g.UseFastBrain = useFB
	c, runID := enabledGuardianController(t, fake, clock, res, g)
	h := &triageHarness{t: t, c: c, fake: fake, clock: clock, runID: runID, t0: t0}
	c.triageFn = func(_ context.Context, in fastbrain.StallInput) fastbrain.StallDiagnosis {
		h.calls.Add(1)
		return diag(in)
	}
	return h
}

func (h *triageHarness) tick(d time.Duration) {
	h.clock.add(d)
	h.c.guardianTick(context.Background())
}

// settle waits for the in-flight diagnosis to be delivered.
func (h *triageHarness) settle() {
	h.t.Helper()
	require.Eventually(h.t, func() bool {
		h.c.mu.Lock()
		defer h.c.mu.Unlock()
		return h.c.runs[h.runID].triage.result != nil
	}, 2*time.Second, time.Millisecond)
}

// stall drives a stale-heartbeat run through launch → deliver → apply.
func (h *triageHarness) stall() {
	h.tick(11 * time.Minute) // stale: launches the diagnosis, defers
	h.settle()
	h.tick(time.Minute) // applies it
}

func (h *triageHarness) audited(sub string) int {
	n := 0
	for _, a := range h.fake.audits {
		if strings.Contains(a, sub) {
			n++
		}
	}
	return n
}

func model(a fastbrain.StallAction, conf float64) func(fastbrain.StallInput) fastbrain.StallDiagnosis {
	return func(fastbrain.StallInput) fastbrain.StallDiagnosis {
		return fastbrain.StallDiagnosis{Action: a, Text: "model says hi", Confidence: conf, Source: "model", Rationale: "r"}
	}
}

func TestTriageOffRestoresLadder(t *testing.T) {
	h := newTriageHarness(t, false, model(fastbrain.ActionWait, 1))
	h.tick(11 * time.Minute)
	require.Len(t, h.fake.nudges, 1)
	require.Contains(t, h.fake.nudges[0], "autopilot guardian")
	require.Zero(t, h.calls.Load())
}

func TestTriageWaitDefersWithinBounds(t *testing.T) {
	h := newTriageHarness(t, true, model(fastbrain.ActionWait, 0.9))
	h.tick(11 * time.Minute)
	require.Empty(t, h.fake.nudges, "the tick that launches triage escalates nothing")
	h.settle()
	h.tick(time.Minute)
	require.Empty(t, h.fake.nudges, "wait defers the nudge")
	require.Equal(t, 1, h.audited("autopilot.guardian_diagnosis"))

	// Waits 2 and 3, each after the previous deferral elapsed.
	for i := 0; i < 2; i++ {
		h.tick(11 * time.Minute)
		h.settle()
		h.tick(time.Minute)
		require.Empty(t, h.fake.nudges)
	}
	// 4th wait is rewritten to the generic nudge.
	h.tick(11 * time.Minute)
	h.settle()
	h.tick(time.Minute)
	require.Len(t, h.fake.nudges, 1)
	require.Contains(t, h.fake.nudges[0], "autopilot guardian")
}

func TestTriageWaitTotalBound(t *testing.T) {
	h := newTriageHarness(t, true, model(fastbrain.ActionWait, 0.9))
	h.c.guardian.MaxWaitTotal = 15 * time.Minute // second wait is clamped, third exceeds
	h.stall()                                    // wait 1: 10m
	require.Empty(t, h.fake.nudges)
	h.tick(11 * time.Minute)
	h.settle()
	h.tick(time.Minute) // wait 2: 5m (clamped)
	require.Empty(t, h.fake.nudges)
	h.tick(6 * time.Minute)
	h.settle()
	h.tick(time.Minute) // total exhausted ⇒ nudge
	require.Len(t, h.fake.nudges, 1)
}

func TestTriageNudgeUsesModelText(t *testing.T) {
	h := newTriageHarness(t, true, model(fastbrain.ActionNudge, 0.5))
	h.stall()
	require.Len(t, h.fake.nudges, 1)
	require.Contains(t, h.fake.nudges[0], "model says hi")
}

func TestTriageRestartAndRotateNeedConfidence(t *testing.T) {
	// Low confidence downgrades to the mechanical rung (stage 1 nudge).
	h := newTriageHarness(t, true, model(fastbrain.ActionRestart, 0.5))
	h.stall()
	require.Empty(t, h.fake.rotated)
	require.Len(t, h.fake.nudges, 1)

	h = newTriageHarness(t, true, model(fastbrain.ActionRestart, 0.9))
	h.stall()
	require.Len(t, h.fake.rotated, 1)
	require.Equal(t, "a", h.fake.rotated[0].Backend, "restart stays on the same backend")
	require.Empty(t, h.fake.nudges)

	h = newTriageHarness(t, true, model(fastbrain.ActionRotate, 0.9))
	h.stall()
	require.Len(t, h.fake.rotated, 1)
	require.Equal(t, "b", h.fake.rotated[0].Backend, "rotate moves to the next backend")
}

func TestTriageDelegatingActions(t *testing.T) {
	cases := map[fastbrain.StallAction]string{
		fastbrain.ActionResolvePrompt:   "resolve:brain-1",
		fastbrain.ActionResumeRateLimit: "resume:brain-1",
		fastbrain.ActionRedeliverPrompt: "redeliver:brain-1",
	}
	for act, want := range cases {
		h := newTriageHarness(t, true, model(act, 0.9))
		h.stall()
		require.Equal(t, []string{want}, h.fake.evCalls, act)
		require.Empty(t, h.fake.nudges, "the manager gets one heartbeat window instead of a nudge")
		// Still stale inside the window: nothing escalates; after it, triage re-runs.
		h.tick(5 * time.Minute)
		require.Empty(t, h.fake.nudges, act)
		require.Equal(t, 1, h.audited("autopilot.guardian_diagnosis"))
	}

	// A failing delegate falls open to the ladder.
	h := newTriageHarness(t, true, model(fastbrain.ActionResolvePrompt, 0.9))
	h.fake.evActErr = ErrAgentNotFound
	h.stall()
	require.Len(t, h.fake.nudges, 1)
}

func TestTriageCallResolverSeam(t *testing.T) {
	h := newTriageHarness(t, true, model(fastbrain.ActionCallResolver, 0.9))
	rr := &resolverFake{guardianFake: h.fake}
	h.c.SetRuntime(rr)
	h.stall()
	require.Len(t, rr.spawned, 1)
	require.Equal(t, h.runID, rr.spawned[0].RunID)
	require.Equal(t, BlockerManagerStall, rr.spawned[0].Class)
	require.Empty(t, h.fake.nudges)

	// Runtime cannot spawn resolvers ⇒ mechanical ladder.
	h = newTriageHarness(t, true, model(fastbrain.ActionCallResolver, 0.9))
	h.stall()
	require.Len(t, h.fake.nudges, 1)
}

func TestTriageFailOpenAndMechanicalRunLadder(t *testing.T) {
	for _, d := range []fastbrain.StallDiagnosis{
		{Action: fastbrain.ActionMechanical, Source: "heuristic"},
		{Action: fastbrain.ActionMechanical, Source: "failopen", FailOpen: fastbrain.FailOpenTimeout},
	} {
		d := d
		h := newTriageHarness(t, true, func(fastbrain.StallInput) fastbrain.StallDiagnosis { return d })
		h.stall()
		require.Len(t, h.fake.nudges, 1)
		require.Contains(t, h.fake.nudges[0], "autopilot guardian")
		require.Equal(t, 1, h.audited("autopilot.guardian_diagnosis"))
	}
}

func TestTriageStaleResultDiscardedAfterManagerReplaced(t *testing.T) {
	h := newTriageHarness(t, true, model(fastbrain.ActionNudge, 0.9))
	h.tick(11 * time.Minute)
	h.settle()
	h.c.mu.Lock()
	h.c.runs[h.runID].brain.AgentID = "brain-replaced"
	h.c.mu.Unlock()
	h.tick(time.Minute)
	require.Empty(t, h.fake.nudges, "a diagnosis for a replaced manager is not applied")
	require.Equal(t, 1, h.audited("autopilot.guardian_diagnosis"))
}

func TestTriageStaleResultDiscardedAfterStageChanged(t *testing.T) {
	h := newTriageHarness(t, true, model(fastbrain.ActionNudge, 0.9))
	h.tick(11 * time.Minute)
	h.settle()
	h.c.mu.Lock()
	h.c.runs[h.runID].healStage = stageNudged
	h.c.runs[h.runID].healNextAt = h.clock.now().Add(time.Minute) // a real nudged run always has a next step
	h.c.mu.Unlock()
	h.tick(time.Minute)
	require.Empty(t, h.fake.nudges)
}

func TestTriageTimeoutFailsOpenViaEngine(t *testing.T) {
	h := newTriageHarness(t, true, nil)
	h.c.triageFn = nil // real DiagnoseStall path with a nil engine ⇒ no_runner fail-open
	h.fake.evidence = AgentEvidence{PaneTail: "working on it..."}
	h.stall()
	require.Len(t, h.fake.nudges, 1)
	require.Equal(t, 1, h.audited("autopilot.guardian_diagnosis"))
}

// The model call must run with c.mu released, and only one is in flight per run.
func TestTriageNoLockHeldAcrossModelCall(t *testing.T) {
	h := newTriageHarness(t, true, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	h.c.triageFn = func(context.Context, fastbrain.StallInput) fastbrain.StallDiagnosis {
		h.calls.Add(1)
		close(started)
		<-release
		return fastbrain.StallDiagnosis{Action: fastbrain.ActionMechanical, Source: "failopen"}
	}
	h.tick(11 * time.Minute)
	<-started
	require.True(t, h.c.mu.TryLock(), "c.mu must be free while the model call runs")
	h.c.mu.Unlock()
	h.tick(time.Minute) // still in flight: no second call, no escalation
	require.EqualValues(t, 1, h.calls.Load())
	require.Empty(t, h.fake.nudges)
	close(release)
	h.settle()
	h.tick(time.Minute)
	require.Len(t, h.fake.nudges, 1)
}
