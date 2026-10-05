package autopilot

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

// The invariant (spec §J.2) is checked on every guardian tick of every test in
// the package: a run that is not complete/paused/stopped/parked must carry a
// scheduled next action. Violations fail the package from TestMain.
var (
	violationsMu sync.Mutex
	violations   []string
)

func TestMain(m *testing.M) {
	invariantHook = func(runID, v string) {
		violationsMu.Lock()
		defer violationsMu.Unlock()
		var tests []string
		for _, line := range strings.Split(string(debug.Stack()), "\n") {
			if i := strings.Index(line, "autopilot.Test"); i >= 0 {
				tests = append(tests, strings.Fields(line[i:])[0])
			}
		}
		violations = append(violations, runID+": "+v+" in "+strings.Join(tests, ","))
	}
	code := m.Run()
	violationsMu.Lock()
	defer violationsMu.Unlock()
	if len(violations) > 0 && code == 0 {
		fmt.Fprintln(os.Stderr, "next-step invariant violated:\n  "+strings.Join(violations, "\n  "))
		code = 1
	}
	os.Exit(code)
}

// limitFake adds the LimitRuntime seam to the guardian fake.
type limitFake struct {
	*guardianFake
	switchRes  map[string]LimitSwitch // agentID → result
	switchErr  map[string]error
	switchCall []string
}

func (f *limitFake) SwitchLimited(_ context.Context, id string) (LimitSwitch, error) {
	f.switchCall = append(f.switchCall, id)
	return f.switchRes[id], f.switchErr[id]
}

func newLimitHarness(t *testing.T) (*Controller, *limitFake, *fakeClock, string) {
	t.Helper()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	base := newGuardianFake()
	res := &roundRobinResolver{backends: []string{"a", "b"}, tiers: []backendstore.ModelTier{"free", "subscription"}}
	c, runID := enabledGuardianController(t, base, clock, res, testGuardian())
	f := &limitFake{guardianFake: base, switchRes: map[string]LimitSwitch{}, switchErr: map[string]error{}}
	c.SetRuntime(f)
	return c, f, clock, runID
}

func (f *limitFake) setRoster(runID, mgr string, workerState string) {
	f.roster[runID] = []AgentInfo{
		{ID: mgr, State: "working"},
		{ID: "w1", Name: "w1", State: workerState},
	}
}

func hasAudit(f *limitFake, prefix string) bool {
	for _, a := range f.audits {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func TestLimitWorkerSwitchesToAlternate(t *testing.T) {
	c, f, clock, runID := newLimitHarness(t)
	mgr := c.runs[runID].brain.AgentID
	f.setRoster(runID, mgr, "rate_limited")
	f.switchRes["w1"] = LimitSwitch{From: "a", To: "b"}

	c.limitTick(context.Background())
	require.Equal(t, []string{"w1"}, f.switchCall)
	require.True(t, hasAudit(f, "autopilot_limit_switched:w1"))
	require.Empty(t, f.evCalls, "no resume when an alternate exists")

	// While the swap settles the agent is not asked again.
	clock.add(time.Minute)
	c.limitTick(context.Background())
	require.Len(t, f.switchCall, 1)

	// The swap landed: the agent is no longer limited, bookkeeping clears.
	f.setRoster(runID, mgr, "working")
	c.limitTick(context.Background())
	require.Empty(t, c.runs[runID].resting)
}

func TestLimitWorkerNoAlternateWaitsAndResumesAtReset(t *testing.T) {
	c, f, clock, runID := newLimitHarness(t)
	mgr := c.runs[runID].brain.AgentID
	f.setRoster(runID, mgr, "rate_limited")
	reset := clock.now().Add(time.Hour)
	f.switchRes["w1"] = LimitSwitch{From: "a", Reset: reset}
	f.switchErr["w1"] = ErrNoAlternateBackend

	c.limitTick(context.Background())
	require.True(t, hasAudit(f, "autopilot_limit_resume_scheduled:w1"))
	st, _ := c.LookupRun(runID)
	require.Equal(t, reset.Add(limitResumeBuffer).UTC().Format(time.RFC3339), st.RestingUntil)
	require.NotNil(t, st.NextStep)
	require.Equal(t, st.RestingUntil, st.NextStep.At)

	clock.add(30 * time.Minute)
	c.limitTick(context.Background())
	require.Empty(t, f.evCalls, "not before the reset")
	require.Len(t, f.switchCall, 1)

	clock.add(31 * time.Minute) // past reset + buffer
	c.limitTick(context.Background())
	require.Equal(t, []string{"resume:w1"}, f.evCalls)

	// Still limited after the resume: one re-check, then it is rescheduled
	// (never left resting without a next action).
	clock.add(limitRecheckDelay + time.Second)
	f.switchRes["w1"] = LimitSwitch{From: "a", Reset: clock.now().Add(2 * time.Hour)}
	c.limitTick(context.Background())
	require.Len(t, f.switchCall, 2)
	ent := c.runs[runID].resting["w1"]
	require.NotNil(t, ent)
	require.True(t, ent.until.After(clock.now()))
}

func TestLimitNoResetTimeUsesFallback(t *testing.T) {
	c, f, clock, runID := newLimitHarness(t)
	f.setRoster(runID, c.runs[runID].brain.AgentID, "rate_limited")
	f.switchErr["w1"] = ErrNoAlternateBackend
	c.limitTick(context.Background())
	ent := c.runs[runID].resting["w1"]
	require.True(t, ent.fallback)
	require.Equal(t, clock.now().Add(limitFallbackRetry), ent.until)
}

func TestLimitManagerSwitchesAndRestingManagerNotEscalated(t *testing.T) {
	c, f, clock, runID := newLimitHarness(t)
	mgr := c.runs[runID].brain.AgentID
	f.roster[runID] = []AgentInfo{{ID: mgr, State: "rate_limited"}}
	f.switchRes[mgr] = LimitSwitch{From: "a", To: "b"}
	c.limitTick(context.Background())
	require.True(t, hasAudit(f, "autopilot_limit_switched:"+mgr))

	// No alternate: the manager rests until reset; its silence is not a wedge.
	c2, f2, clock2, runID2 := newLimitHarness(t)
	mgr2 := c2.runs[runID2].brain.AgentID
	f2.roster[runID2] = []AgentInfo{{ID: mgr2, State: "rate_limited"}}
	f2.switchErr[mgr2] = ErrNoAlternateBackend
	f2.switchRes[mgr2] = LimitSwitch{Reset: clock2.now().Add(3 * time.Hour)}
	c2.limitTick(context.Background())
	clock2.add(11 * time.Minute) // heartbeat now stale
	c2.guardianTick(context.Background())
	require.Empty(t, f2.nudges)
	require.Empty(t, f2.rotated)

	clock2.add(3 * time.Hour)
	c2.limitTick(context.Background())
	require.Equal(t, []string{"resume:" + mgr2}, f2.evCalls)
	_ = clock
}

func TestLimitIgnoredWhenPaused(t *testing.T) {
	c, f, _, runID := newLimitHarness(t)
	f.setRoster(runID, c.runs[runID].brain.AgentID, "rate_limited")
	c.runs[runID].state = StatePaused
	c.limitTick(context.Background())
	require.Empty(t, f.switchCall)
}

func TestNextStepInvariantAcrossStates(t *testing.T) {
	c, _, clock, runID := newLimitHarness(t)
	r := c.runs[runID]
	now := clock.now()
	cases := []struct {
		name  string
		setup func()
		want  bool // next step expected
	}{
		{"active", func() {}, true},
		{"starting", func() { r.state = StateStarting }, true},
		{"nudged", func() { r.state, r.healStage, r.healNextAt = StateHealing, stageNudged, now.Add(time.Minute) }, true},
		{"backoff", func() {
			r.state, r.healStage, r.backoffNextRetry = StateDegraded, stageBackoff, now.Add(time.Minute)
		}, true},
		{"manager lost", func() { r.state, r.healStage, r.brain = StateHealing, stageHealthy, nil }, true},
		{"paused", func() { r.state = StatePaused }, false},
		{"stopped", func() { r.state = StateStopped }, false},
		{"complete", func() { r.state = StateComplete }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			ns := c.nextStepLocked(r, now)
			require.Equal(t, tc.want, ns != nil)
			if ns != nil {
				require.NotEmpty(t, ns.At)
				require.Empty(t, c.nextStepViolation(r, now))
			}
		})
	}
}

func TestNextStepViolationDetected(t *testing.T) {
	c, _, clock, runID := newLimitHarness(t)
	r := c.runs[runID]
	r.state, r.healStage = StateDegraded, stageBackoff // no retry time scheduled
	require.NotEmpty(t, c.nextStepViolation(r, clock.now()))
	r.needsAttention = "bad plan"
	require.Empty(t, c.nextStepViolation(r, clock.now()), "parked runs are exempt")
}
