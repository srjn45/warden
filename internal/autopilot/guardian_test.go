package autopilot

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// guardianFake implements both Runtime and GuardianRuntime so the guardian heal
// ladder can be driven with a fake clock and scriptable heartbeat/context.
type guardianFake struct {
	store   *fakeStore
	sources fakeSources
	nextID  int

	spawned     []BrainSpec
	killed      []string
	nudges      []string
	escalations []string
	installs    int

	spawnErrOn map[string]error     // backend → spawn error (simulate an unavailable backend)
	activity   map[string]time.Time // runID → last brain heartbeat
	ctxLevel   map[string]string    // agentID → context-window level

	roster    map[string][]AgentInfo // runID → live agent roster (overwatch)
	rosterErr error                  // when non-nil, RunAgents fails (overwatch degrade)
	wakes     []string               // overwatch pane-injected wakes ("agentID: msg")
}

func newGuardianFake() *guardianFake {
	return &guardianFake{
		store:      newFakeStore(),
		spawnErrOn: map[string]error{},
		activity:   map[string]time.Time{},
		ctxLevel:   map[string]string{},
		roster:     map[string][]AgentInfo{},
	}
}

// RunAgents backs OverwatchRuntime: the scripted run roster (manager + workers).
func (f *guardianFake) RunAgents(_ context.Context, runID string) ([]AgentInfo, error) {
	if f.rosterErr != nil {
		return nil, f.rosterErr
	}
	return f.roster[runID], nil
}

// WakeAgent backs OverwatchRuntime: records the pane-injected wake.
func (f *guardianFake) WakeAgent(_ context.Context, agentID, msg string) error {
	f.wakes = append(f.wakes, agentID+": "+msg)
	return nil
}

func (f *guardianFake) SpawnBrain(_ context.Context, spec BrainSpec) (BrainHandle, error) {
	if err := f.spawnErrOn[spec.Backend]; err != nil {
		return BrainHandle{}, err
	}
	f.spawned = append(f.spawned, spec)
	f.nextID++
	return BrainHandle{AgentID: fmt.Sprintf("brain-%d", f.nextID), Backend: spec.Backend}, nil
}

func (f *guardianFake) TerminateBrain(_ context.Context, agentID string) error {
	f.killed = append(f.killed, agentID)
	return nil
}

func (f *guardianFake) NewLedger(runID string) *Ledger   { return NewLedger(f.store, runID) }
func (f *guardianFake) DigestSources() DigestSources     { return f.sources }
func (f *guardianFake) NotifyOwner(string, string)       {}
func (f *guardianFake) InstallDefaultAutoApprovePolicy() { f.installs++ }

func (f *guardianFake) BrainActivity(_ context.Context, runID string) (time.Time, bool) {
	t, ok := f.activity[runID]
	return t, ok
}
func (f *guardianFake) BrainContextLevel(_ context.Context, agentID string) string {
	return f.ctxLevel[agentID]
}
func (f *guardianFake) NudgeBrain(_ context.Context, agentID, msg string) error {
	f.nudges = append(f.nudges, agentID+": "+msg)
	return nil
}
func (f *guardianFake) NotifyEscalation(_ string, title, _ string) {
	f.escalations = append(f.escalations, title)
}

// enabledGuardianController spins up an enabled controller on ladder with the
// guardian params, wired to fake at clock, and returns it plus the single run id.
func enabledGuardianController(t *testing.T, fake *guardianFake, clock *fakeClock, ladder BackendLadder, allowPPU bool, g GuardianParams) (*Controller, string) {
	t.Helper()
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	c := NewController(ControllerConfig{
		Plans:          []string{plan},
		BaseDir:        dir,
		Backends:       ladder,
		AllowPayPerUse: allowPPU,
		Guardian:       g,
	}, &fakeEnv{})
	c.setClock(clock.now)
	c.SetRuntime(fake)
	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	return c, st.Runs[0].RunID
}

func testGuardian() GuardianParams {
	return GuardianParams{
		Interval:         time.Minute,
		HeartbeatTimeout: 10 * time.Minute,
		BackoffMin:       30 * time.Second,
		BackoffMax:       time.Hour,
		RotateAtContext:  "critical",
		NotifyEach:       false,
	}
}

// TestGuardianWedgeWalksLadder is the simulated-wedge acceptance: a stale
// heartbeat walks nudge → restart → rotate → backoff (autopilot.md §2.3).
func TestGuardianWedgeWalksLadder(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	ladder := BackendLadder{Free: []string{"a"}, Subscription: []string{"b"}}
	c, _ := enabledGuardianController(t, fake, clock, ladder, false, testGuardian())

	require.Len(t, fake.spawned, 1)
	require.Equal(t, "a", fake.spawned[0].Backend)
	ctx := context.Background()

	// Within the heartbeat timeout: healthy, no action.
	clock.t = t0.Add(5 * time.Minute)
	c.guardianTick(ctx)
	require.Empty(t, fake.nudges, "healthy brain is left alone")

	// Past the timeout with no activity: stage 1 — nudge (no kill).
	clock.t = t0.Add(11 * time.Minute)
	c.guardianTick(ctx)
	require.Len(t, fake.nudges, 1, "wedge ⇒ nudge")
	require.Empty(t, fake.killed)

	// Inside the post-nudge grace window: no further escalation.
	clock.t = t0.Add(15 * time.Minute)
	c.guardianTick(ctx)
	require.Len(t, fake.nudges, 1, "escalation waits out the grace window")

	// Grace elapsed, still wedged: stage 2 — restart on the SAME backend.
	clock.t = t0.Add(22 * time.Minute)
	c.guardianTick(ctx)
	require.Equal(t, []string{"brain-1"}, fake.killed, "restart terminates the old brain")
	require.Len(t, fake.spawned, 2)
	require.Equal(t, "a", fake.spawned[1].Backend, "restart stays on the same backend")

	// Still wedged after the restart's grace: stage 3 — rotate DOWN the ladder.
	clock.t = t0.Add(33 * time.Minute)
	c.guardianTick(ctx)
	require.Equal(t, []string{"brain-1", "brain-2"}, fake.killed)
	require.Len(t, fake.spawned, 3)
	require.Equal(t, "b", fake.spawned[2].Backend, "rotate moves to the next tier")
	require.Equal(t, tierSubscription, c.Status().Runs[0].Brain.Tier)

	// Ladder exhausted: stage 4 — backoff (never parks, always reschedules).
	clock.t = t0.Add(44 * time.Minute)
	c.guardianTick(ctx)
	bo := c.Status().Runs[0].Backoff
	require.NotNil(t, bo, "exhausted ladder ⇒ backoff")
	require.Equal(t, 1, bo.Stage)
	require.NotEmpty(t, bo.NextRetryAt, "backoff always schedules a next retry — never parks")
	require.NotEmpty(t, fake.escalations, "a stall notifies the owner")
}

// TestGuardianRecoversOnHeartbeat proves a fresh heartbeat clears the heal ladder
// (autopilot.md §2.3 healthy transition).
func TestGuardianRecoversOnHeartbeat(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, BackendLadder{Free: []string{"a"}}, false, testGuardian())
	ctx := context.Background()

	// Wedge → nudge.
	clock.t = t0.Add(11 * time.Minute)
	c.guardianTick(ctx)
	require.Len(t, fake.nudges, 1)
	require.Equal(t, StateHealing, c.runs[runID].state)

	// The brain acts (fresh heartbeat) → the ladder resets to healthy.
	fake.activity[runID] = clock.t
	clock.t = t0.Add(12 * time.Minute)
	c.guardianTick(ctx)
	require.Equal(t, stageHealthy, c.runs[runID].healStage, "a heartbeat recovers the brain")
	require.Equal(t, StateActive, c.runs[runID].state)
	require.Nil(t, c.Status().Runs[0].Backoff)
}

// TestGuardianBackoffCapAndNeverParks exercises the capped-exponential backoff in
// isolation: it grows, clamps at backoff_max, and always reschedules (§2.3).
func TestGuardianBackoffCapAndNeverParks(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	g := testGuardian()
	g.BackoffMin = time.Minute
	g.BackoffMax = 8 * time.Minute
	c := NewController(ControllerConfig{Backends: BackendLadder{Free: []string{"a"}}, Guardian: g}, &fakeEnv{})
	c.setClock(clock.now)
	c.SetRuntime(fake)

	r := &run{runID: "ap-x", tried: map[string]bool{}}
	wantWaits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 8 * time.Minute}
	for i, want := range wantWaits {
		before := clock.t
		c.enterBackoff(fake, r, before, false)
		require.Equal(t, i+1, r.backoffStage)
		require.Equal(t, before.Add(want), r.backoffNextRetry, "capped-exponential step %d", i+1)
		require.True(t, r.backoffNextRetry.After(before), "never parks — retry is always in the future")
	}
	require.NotEmpty(t, fake.escalations)
}

// TestGuardianBackoffWakesAtEarliestReset proves backoff wakes as soon as a limited
// backend frees, rather than sleeping a full interval past a known reset (§7).
func TestGuardianBackoffWakesAtEarliestReset(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	g := testGuardian()
	g.BackoffMin = time.Hour // a long backoff...
	g.BackoffMax = 6 * time.Hour
	c := NewController(ControllerConfig{Backends: BackendLadder{Free: []string{"a"}}, Guardian: g}, &fakeEnv{})
	c.setClock(clock.now)
	c.SetRuntime(fake)

	c.MarkBackendLimited("a", t0.Add(20*time.Minute)) // ...but a backend frees in 20m
	r := &run{runID: "ap-x", tried: map[string]bool{}}
	c.enterBackoff(fake, r, t0, false)
	require.Equal(t, t0.Add(20*time.Minute), r.backoffNextRetry, "wake at the earliest reset, not the full backoff")
}

// TestGuardianGateOnlyNotification proves that when the only thing left is a
// gated pay-per-use backend, backoff carries the distinct gate signal (§7).
func TestGuardianGateOnlyNotification(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	// Free tier limited, pay_per_use present but gate off ⇒ selection is gateOnly.
	ladder := BackendLadder{Free: []string{"a"}, PayPerUse: []string{"gpt"}}
	c, runID := enabledGuardianController(t, fake, clock, ladder, false, testGuardian())
	ctx := context.Background()
	c.MarkBackendLimited("a", t0.Add(2*time.Hour))

	// Drive it straight to backoff: nudge, restart, then exhausted → gateOnly.
	c.runs[runID].tried["a"] = true // pretend the free backend was already tried this cycle
	c.runs[runID].healStage = stageRotated
	c.runs[runID].healNextAt = time.Time{}
	clock.t = t0.Add(11 * time.Minute)
	c.guardianTick(ctx)

	require.Contains(t, c.Status().Runs[0].Backoff.LastError, "allow_pay_per_use", "the gate is the distinct signal")
}

// TestGuardianPlannedRotationOnContext proves a healthy brain whose context reaches
// the threshold is cold-started, and the cooldown prevents thrashing (§2.3).
func TestGuardianPlannedRotationOnContext(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, BackendLadder{Free: []string{"a"}}, false, testGuardian())
	ctx := context.Background()

	// The brain is healthy (fresh heartbeat) but its context is critical.
	fake.activity[runID] = t0.Add(3 * time.Minute)
	fake.ctxLevel["brain-1"] = "critical"
	clock.t = t0.Add(4 * time.Minute)
	c.guardianTick(ctx)
	require.Equal(t, []string{"brain-1"}, fake.killed, "context critical ⇒ planned cold-start rotation")
	require.Len(t, fake.spawned, 2)

	// The freshly rotated brain is also critical, but the cooldown suppresses an
	// immediate re-rotation.
	fake.activity[runID] = clock.t
	fake.ctxLevel["brain-2"] = "critical"
	c.guardianTick(ctx)
	require.Len(t, fake.spawned, 2, "cooldown prevents thrashing")

	// Past the cooldown, it rotates again.
	clock.t = t0.Add(20 * time.Minute)
	fake.activity[runID] = clock.t
	c.guardianTick(ctx)
	require.Len(t, fake.spawned, 3, "planned rotation resumes after the cooldown")
}

// TestGuardianHonorsKillSwitch proves a disabled controller supervises nothing,
// and that a runtime without GuardianRuntime makes the tick a no-op.
func TestGuardianHonorsKillSwitch(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, _ := enabledGuardianController(t, fake, clock, BackendLadder{Free: []string{"a"}}, false, testGuardian())
	ctx := context.Background()

	c.Disable(ctx, "")
	clock.t = t0.Add(30 * time.Minute)
	c.guardianTick(ctx) // wedged by the clock, but disabled ⇒ no heal
	require.Empty(t, fake.nudges, "a disabled controller heals nothing")

	// A runtime that does not implement GuardianRuntime is never guardian-managed.
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	plainRT := newFakeRuntime()
	c2 := NewController(ControllerConfig{Plans: []string{plan}, BaseDir: dir, Backends: BackendLadder{Free: []string{"a"}}, Guardian: testGuardian()}, &fakeEnv{})
	c2.setClock(clock.now)
	c2.SetRuntime(plainRT)
	_, err := c2.Enable(ctx, "")
	require.NoError(t, err)
	clock.t = t0.Add(90 * time.Minute)
	c2.guardianTick(ctx) // must be a no-op — no GuardianRuntime
	require.Empty(t, plainRT.killed, "a non-guardian runtime is never healed")
}
