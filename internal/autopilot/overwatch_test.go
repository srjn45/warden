package autopilot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// overwatchSetup enables a single-run controller wired to a fresh guardian fake
// (which also backs OverwatchRuntime) and returns it plus the run id and the
// spawned manager's (brain's) agent id. The manager spawns at the clock's
// current instant, which floors the overwatch nudge clock (cold-start).
func overwatchSetup(t *testing.T, clock *fakeClock) (*Controller, *guardianFake, string, string) {
	t.Helper()
	fake := newGuardianFake()
	ladder := BackendLadder{Free: []string{"a"}}
	c, runID := enabledGuardianController(t, fake, clock, ladder, false, testGuardian())
	managerID := c.runs[runID].brain.AgentID // "brain-1"
	return c, fake, runID, managerID
}

// manager builds the roster entry for the run's manager (the brain) in state.
func manager(id, state string) AgentInfo { return AgentInfo{ID: id, Name: "manager", State: state} }

func TestOverwatchWakesIdleManagerOnWaitingWorker(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	ctx := context.Background()

	// Manager idle, one worker waiting on input, one still working.
	fake.roster[runID] = []AgentInfo{
		manager(mgr, "idle"),
		{ID: "w1", Name: "api", State: "waiting_for_input"},
		{ID: "w2", Name: "db", State: "working"},
	}

	// Inside the cold-start floor (manager spawned at t0): no wake yet.
	clock.t = t0.Add(time.Minute)
	c.overwatchTick(ctx)
	require.Empty(t, fake.wakes, "a freshly-spawned manager is not nudged (cold-start floor)")

	// Past the min gap since spawn: the event wake fires.
	clock.t = t0.Add(overwatchMinGap + time.Minute)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 1, "an idle manager with a waiting worker is woken")
	require.Contains(t, fake.wakes[0], mgr+": ")
	require.Contains(t, fake.wakes[0], overwatchNudgePrefix)
	require.Contains(t, fake.wakes[0], "w1", "the waiting worker is named")
	require.NotContains(t, fake.wakes[0], "w2", "a busy worker is not listed as needy")
	require.Equal(t, 1, c.Status().Runs[0].WorkersInFlight, "one busy worker tracked in-flight")
	require.Empty(t, fake.nudges, "the overwatch wakes the pane; it does not use the mailbox")
}

func TestOverwatchSkipsBusyManager(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)

	// Manager actively working, even though a worker is waiting — never interrupt
	// it. The clock is well past every gate so busyness is the only reason.
	fake.roster[runID] = []AgentInfo{
		manager(mgr, "working"),
		{ID: "w1", State: "waiting_for_input"},
	}
	clock.t = t0.Add(2 * overwatchPeriod)

	c.overwatchTick(context.Background())
	require.Empty(t, fake.wakes, "a busy manager is never woken")
}

func TestOverwatchDebouncesEventWakes(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	ctx := context.Background()
	fake.roster[runID] = []AgentInfo{manager(mgr, "idle"), {ID: "w1", State: "idle"}}

	clock.t = t0.Add(overwatchMinGap + time.Minute)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 1, "first eligible tick wakes")
	last := clock.t

	// Another tick inside the min-gap window: debounced.
	clock.t = last.Add(2 * time.Minute)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 1, "event wakes are debounced within the min gap")

	// Past the min gap, worker still needy: wake again.
	clock.t = last.Add(overwatchMinGap + time.Second)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 2, "a fresh wake fires once the min gap elapses")
}

func TestOverwatchPeriodicWakeForIdleManager(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	ctx := context.Background()

	// Manager idle, no needy workers at all — only the periodic check-in applies.
	fake.roster[runID] = []AgentInfo{manager(mgr, "idle")}

	// Before the period elapses (measured from spawn): nothing.
	clock.t = t0.Add(30 * time.Minute)
	c.overwatchTick(ctx)
	require.Empty(t, fake.wakes, "no periodic wake before the period elapses")

	// Period elapsed since spawn: the heartbeat check-in fires.
	clock.t = t0.Add(overwatchPeriod + time.Minute)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 1, "the periodic heartbeat fires once the period elapses")
	require.Contains(t, fake.wakes[0], "periodic check-in")

	// And again only after another full period.
	clock.t = t0.Add(overwatchPeriod + 30*time.Minute)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 1)
	clock.t = t0.Add(2*overwatchPeriod + 2*time.Minute)
	c.overwatchTick(ctx)
	require.Len(t, fake.wakes, 2, "the next heartbeat waits out a full period")
}

func TestOverwatchSkipsNonActiveRun(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	fake.roster[runID] = []AgentInfo{manager(mgr, "idle"), {ID: "w1", State: "waiting_for_input"}}
	clock.t = t0.Add(2 * overwatchPeriod)

	// A healing/degraded run is the guardian's business — the overwatch stays out.
	c.runs[runID].state = StateHealing

	c.overwatchTick(context.Background())
	require.Empty(t, fake.wakes, "a non-active run is not overwatched (guardian owns it)")
	require.Equal(t, 0, c.Status().Runs[0].WorkersInFlight, "no busy workers in the roster")
}

func TestOverwatchTracksWorkersInFlightRegardlessOfState(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	// Busy manager (no wake) but two working workers — the count still refreshes.
	fake.roster[runID] = []AgentInfo{
		manager(mgr, "working"),
		{ID: "w1", State: "working"},
		{ID: "w2", State: "spawning"},
		{ID: "w3", State: "done"},
	}
	clock.t = t0.Add(2 * overwatchPeriod)

	c.overwatchTick(context.Background())
	require.Empty(t, fake.wakes)
	require.Equal(t, 2, c.Status().Runs[0].WorkersInFlight, "working + spawning workers are in-flight; done is not")
}

func TestOverwatchRosterErrorKeepsLastCount(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	fake.roster[runID] = []AgentInfo{manager(mgr, "idle"), {ID: "w1", State: "working"}}
	clock.t = t0.Add(overwatchMinGap + time.Minute)
	c.overwatchTick(context.Background())
	require.Equal(t, 1, c.Status().Runs[0].WorkersInFlight)
	wakesBefore := len(fake.wakes)

	// A roster read failure must not wake nor reset the cached count.
	fake.rosterErr = errors.New("store down")
	clock.t = t0.Add(2 * overwatchPeriod)
	c.overwatchTick(context.Background())
	require.Len(t, fake.wakes, wakesBefore, "no new wake on a roster read error")
	require.Equal(t, 1, c.Status().Runs[0].WorkersInFlight, "cached count survives a read error")
}

func TestOverwatchDisabledControllerIsNoOp(t *testing.T) {
	t0 := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	c, fake, runID, mgr := overwatchSetup(t, clock)
	fake.roster[runID] = []AgentInfo{manager(mgr, "idle"), {ID: "w1", State: "idle"}}
	clock.t = t0.Add(2 * overwatchPeriod)

	c.Disable(context.Background())
	c.overwatchTick(context.Background())
	require.Empty(t, fake.wakes, "a disabled controller overwatches nothing")
}

func TestComposeOverwatchNudgeCapsList(t *testing.T) {
	needy := make([]AgentInfo, overwatchNudgeListMax+3)
	for i := range needy {
		needy[i] = AgentInfo{ID: string(rune('a'+i)) + "-id", Name: "", State: "idle"}
	}
	msg := composeOverwatchNudge(needy)
	require.Contains(t, msg, "(+3 more)", "the tail beyond the cap collapses")
	require.Equal(t, overwatchNudgeListMax, strings.Count(msg, ", idle)"), "only the cap is enumerated")
}
