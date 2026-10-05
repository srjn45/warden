package autopilot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

type wdHarness struct {
	t     *testing.T
	c     *Controller
	fake  *guardianFake
	clock *fakeClock
	runID string
	t0    time.Time
	steps int
}

func newWDHarness(t *testing.T, workers ...AgentInfo) *wdHarness {
	t.Helper()
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	res := &roundRobinResolver{
		backends: []string{"a", "b"},
		tiers:    []backendstore.ModelTier{"free", "subscription"},
	}
	g := testGuardian()
	g.WatchdogWindow = time.Hour
	c, runID := enabledGuardianController(t, fake, clock, res, g)
	h := &wdHarness{t: t, c: c, fake: fake, clock: clock, runID: runID, t0: t0}
	fake.roster[runID] = append([]AgentInfo{{ID: "brain-1", State: "waiting_for_input"}}, workers...)
	h.tick(0) // seed the progress clock + fingerprint
	return h
}

// tick advances the clock, keeps the manager heartbeat fresh (the watchdog's
// whole premise), and runs one guardian pass.
func (h *wdHarness) tick(d time.Duration) {
	h.clock.add(d)
	h.fake.activity[h.runID] = h.clock.now()
	h.c.guardianTick(context.Background())
}

// progress makes a ledger task state change (each call moves to a new state).
func (h *wdHarness) progress() {
	h.t.Helper()
	states := []LedgerState{LedgerPending, LedgerAssigned, LedgerInProgress, LedgerPROpen, LedgerGated, LedgerLanded}
	h.steps++
	st := states[h.steps%len(states)]
	require.NoError(h.t, h.c.runtime.NewLedger(h.runID).WriteTasks([]LedgerTask{{ID: "t1", State: st}}, "test"))
}

func (h *wdHarness) status() RunStatus { return h.c.Status().Runs[0] }

func (h *wdHarness) watchdogNudges() int {
	n := 0
	for _, m := range h.fake.nudges {
		if strings.Contains(m, "autopilot watchdog") {
			n++
		}
	}
	return n
}

func TestWatchdogNoEscalationWhileAgentWorking(t *testing.T) {
	h := newWDHarness(t, AgentInfo{ID: "w1", State: "working"})
	for i := 0; i < 8; i++ {
		h.tick(30 * time.Minute)
	}
	require.Empty(t, h.fake.nudges, "a working agent means the run is not wedged")
	require.Empty(t, h.fake.rotated)
}

func TestWatchdogNoEscalationWithRecentProgress(t *testing.T) {
	h := newWDHarness(t)
	for i := 0; i < 8; i++ {
		h.tick(30 * time.Minute)
		h.progress()
	}
	require.Empty(t, h.fake.nudges)
	require.Equal(t, WatchdogIdle, h.status().Watchdog)
	require.NotEmpty(t, h.status().LastProgressAt)
}

func TestWatchdogHeartbeatAndOverwatchAreNotProgress(t *testing.T) {
	h := newWDHarness(t)
	h.tick(30 * time.Minute)
	require.Equal(t, WatchdogIdle, h.status().Watchdog)
	h.tick(31 * time.Minute) // fresh heartbeat every tick, still no progress
	require.Equal(t, 1, h.watchdogNudges(), "heartbeating alone does not keep the window open")
}

func TestWatchdogEscalatesAfterWindowThenParks(t *testing.T) {
	h := newWDHarness(t)
	h.progress()
	h.tick(time.Minute) // register the ledger change as progress
	base := h.clock.now()

	h.tick(59 * time.Minute)
	require.Empty(t, h.fake.nudges, "inside the window")

	h.tick(2 * time.Minute) // window elapsed
	require.Equal(t, 1, h.watchdogNudges())
	require.Contains(t, h.fake.nudges[0], "t1", "nudge names the stalled task")
	require.Equal(t, WatchdogEscalating, h.status().Watchdog)

	h.tick(5 * time.Minute) // inside grace
	require.Equal(t, 1, h.watchdogNudges(), "shares the heal grace — no double step")
	require.Empty(t, h.fake.rotated)

	h.tick(6 * time.Minute) // grace over: restart
	require.Len(t, h.fake.rotated, 1)
	require.Equal(t, "a", h.fake.rotated[0].Backend)

	h.tick(11 * time.Minute) // rotate
	require.Len(t, h.fake.rotated, 2)
	require.Equal(t, "b", h.fake.rotated[1].Backend)
	require.Equal(t, WatchdogEscalating, h.status().Watchdog)

	h.tick(11 * time.Minute) // ladder exhausted → park
	st := h.status()
	require.Equal(t, WatchdogParked, st.Watchdog)
	require.Contains(t, st.NeedsAttention, "no_progress")
	require.Contains(t, st.NeedsAttention, "wd plan restart")
	require.Greater(t, h.clock.now().Sub(base), time.Hour)

	notified := func() int {
		n := 0
		for _, e := range h.fake.escalations {
			if strings.Contains(e, "no_progress") {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, notified())
	nudges, rotated := len(h.fake.nudges), len(h.fake.rotated)
	for i := 0; i < 5; i++ {
		h.tick(time.Hour)
	}
	require.Equal(t, 1, notified(), "operator notified once")
	require.Len(t, h.fake.nudges, nudges)
	require.Len(t, h.fake.rotated, rotated, "a parked run is left alone")
}

func TestWatchdogProgressClearsEscalation(t *testing.T) {
	h := newWDHarness(t)
	h.tick(61 * time.Minute)
	require.Equal(t, 1, h.watchdogNudges())
	require.Equal(t, WatchdogEscalating, h.status().Watchdog)

	h.progress()
	h.tick(time.Minute)
	st := h.status()
	require.Equal(t, WatchdogIdle, st.Watchdog)
	require.Equal(t, StateActive, st.State)
	require.Equal(t, stageHealthy, h.c.runs[h.runID].healStage)

	h.tick(30 * time.Minute)
	require.Equal(t, 1, h.watchdogNudges(), "window restarted from the progress")
}

func TestWatchdogDisabled(t *testing.T) {
	h := newWDHarness(t)
	h.c.guardian.WatchdogDisabled = true
	h.tick(5 * time.Hour)
	require.Empty(t, h.fake.nudges)
	require.Equal(t, WatchdogDisabled, h.status().Watchdog)
}

func TestWatchdogSkipsPausedRun(t *testing.T) {
	h := newWDHarness(t)
	_, err := h.c.PauseRun(context.Background(), h.runID)
	require.NoError(t, err)
	h.tick(5 * time.Hour)
	require.Empty(t, h.fake.nudges)
}

func TestWatchdogPersistsAcrossControllerRebuild(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	data, dir := t.TempDir(), t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	g := testGuardian()
	g.WatchdogWindow = time.Hour
	cfg := ControllerConfig{Plans: []string{plan}, BaseDir: dir, DataDir: data, Resolver: cyclicResolver("a", "free"), Guardian: g}

	c1 := NewController(cfg, &fakeEnv{})
	c1.setClock(clock.now)
	c1.SetRuntime(fake)
	st, err := c1.ReconcileConfiguredPlans(context.Background(), "")
	require.NoError(t, err)
	runID := st.Runs[0].RunID
	fake.roster[runID] = []AgentInfo{{ID: "brain-1", State: "waiting_for_input"}}
	fake.activity[runID] = clock.now()
	c1.guardianTick(context.Background()) // seed + persist
	seeded := c1.Status().Runs[0].LastProgressAt
	require.NotEmpty(t, seeded)
	require.NoError(t, c1.Close())

	clock.add(90 * time.Minute) // longer than the window while "down"
	c2 := NewController(cfg, &fakeEnv{})
	c2.setClock(clock.now)
	c2.SetRuntime(fake)
	r := c2.runs[runID]
	require.NotNil(t, r, "run restored")
	require.Equal(t, seeded, rfc3339OrEmpty(r.lastProgressAt), "daemon restart does not reset the window")
	require.NotEmpty(t, r.progressFP+"x")
}

func TestWatchdogOverlayStateAndSpawnCountAsProgress(t *testing.T) {
	h := newWDHarness(t)
	h.progress() // task t1 enters the ledger array
	h.tick(time.Minute)
	h.tick(50 * time.Minute)
	require.NoError(t, h.c.runtime.NewLedger(h.runID).WriteTaskState("t1", LedgerAssigned, "test"))
	h.tick(time.Minute)
	h.tick(50 * time.Minute) // 51m since overlay change
	require.Empty(t, h.fake.nudges, "overlay write is progress")
	h.fake.roster[h.runID] = append(h.fake.roster[h.runID], AgentInfo{ID: "w9", State: "waiting_for_input"})
	h.tick(time.Minute)
	h.tick(50 * time.Minute)
	require.Empty(t, h.fake.nudges, "a spawned worker is progress")
}
