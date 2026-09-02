package autopilot

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

// guardianFake implements both Runtime and GuardianRuntime so the guardian heal
// ladder can be driven with a fake clock and scriptable heartbeat/context.
type guardianFake struct {
	store   *fakeStore
	sources fakeSources
	nextID  int

	spawned     []BrainSpec
	rotated     []RotateBrainSpec
	killed      []string
	nudges      []string
	escalations []string
	installs    int

	spawnErrOn map[string]error     // backend → spawn error (simulate an unavailable backend)
	rotateErr  error                // when set, RotateBrain fails
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

func (f *guardianFake) RotateBrain(_ context.Context, spec RotateBrainSpec) (BrainHandle, error) {
	if f.rotateErr != nil {
		return BrainHandle{}, f.rotateErr
	}
	f.rotated = append(f.rotated, spec)
	backend := spec.Backend
	if backend == "" {
		backend = "a"
	}
	return BrainHandle{AgentID: spec.AgentID, Backend: backend}, nil
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

// enabledGuardianController spins up an enabled controller with the given resolver
// and guardian params, wired to fake at clock, and returns it plus the single run id.
func enabledGuardianController(t *testing.T, fake *guardianFake, clock *fakeClock, res Resolver, g GuardianParams) (*Controller, string) {
	t.Helper()
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	c := NewController(ControllerConfig{
		Plans:    []string{plan},
		BaseDir:  dir,
		Resolver: res,
		Guardian: g,
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
	// The resolver cycles a→b so the heal ladder walks free→subscription.
	res := &roundRobinResolver{
		backends: []string{"a", "b"},
		tiers:    []backendstore.ModelTier{"free", "subscription"},
	}
	c, _ := enabledGuardianController(t, fake, clock, res, testGuardian())

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

	// Grace elapsed, still wedged: stage 2 — restart on the SAME backend (in-place HotSwap).
	clock.t = t0.Add(22 * time.Minute)
	c.guardianTick(ctx)
	require.Empty(t, fake.killed, "in-place rotation does not terminate the slot")
	require.Len(t, fake.spawned, 1, "in-place rotation does not mint a new session")
	require.Len(t, fake.rotated, 1)
	require.Equal(t, "brain-1", fake.rotated[0].AgentID, "restart keeps the manager slot id")
	require.Equal(t, "a", fake.rotated[0].Backend, "restart stays on the same backend")
	require.Equal(t, RotateReasonHeal, fake.rotated[0].Reason)
	require.Equal(t, "brain-1", c.runs[c.Status().Runs[0].RunID].brain.AgentID)

	// Still wedged after the restart's grace: stage 3 — rotate DOWN the ladder (same slot).
	clock.t = t0.Add(33 * time.Minute)
	c.guardianTick(ctx)
	require.Empty(t, fake.killed)
	require.Len(t, fake.spawned, 1)
	require.Len(t, fake.rotated, 2)
	require.Equal(t, "brain-1", fake.rotated[1].AgentID, "rotate keeps the manager slot id")
	require.Equal(t, "b", fake.rotated[1].Backend, "rotate moves to the next tier")
	require.Equal(t, "brain-1", c.Status().Runs[0].Brain.AgentID)
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
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
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
	c := NewController(ControllerConfig{Resolver: cyclicResolver("a", "free"), Guardian: g}, &fakeEnv{})
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
	c := NewController(ControllerConfig{Resolver: cyclicResolver("a", "free"), Guardian: g}, &fakeEnv{})
	c.setClock(clock.now)
	c.SetRuntime(fake)

	c.MarkBackendLimited("a", t0.Add(20*time.Minute)) // ...but a backend frees in 20m
	r := &run{runID: "ap-x", tried: map[string]bool{}}
	c.enterBackoff(fake, r, t0, false)
	require.Equal(t, t0.Add(20*time.Minute), r.backoffNextRetry, "wake at the earliest reset, not the full backoff")
}

// TestGuardianResolverExhaustedEntersBackoff proves that when the resolver returns
// an error (no eligible backends), the guardian enters backoff with an appropriate
// stall message. The GateOnly concept no longer applies — the resolver owns tier
// eligibility and handles pay_per_use gating internally via its own AllowPaid flag.
func TestGuardianResolverExhaustedEntersBackoff(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	// Resolver that errors — simulates all backends exhausted.
	errRes := &fakeResolver{err: router.ErrAllExhausted}
	c, runID := enabledGuardianController(t, fake, clock, errRes, testGuardian())
	ctx := context.Background()

	// Drive straight to backoff: bypass nudge/restart stages.
	c.runs[runID].healStage = stageRotated
	c.runs[runID].healNextAt = time.Time{}
	clock.t = t0.Add(11 * time.Minute)
	c.guardianTick(ctx)

	require.NotNil(t, c.Status().Runs[0].Backoff, "exhausted resolver ⇒ backoff")
	require.NotEmpty(t, c.Status().Runs[0].Backoff.LastError, "backoff carries an error message")
}

// TestGuardianPlannedRotationOnContext proves a healthy brain whose context reaches
// the threshold is cold-started, and the cooldown prevents thrashing (§2.3).
func TestGuardianPlannedRotationOnContext(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	ctx := context.Background()

	// The brain is healthy (fresh heartbeat) but its context is critical.
	fake.activity[runID] = t0.Add(3 * time.Minute)
	fake.ctxLevel["brain-1"] = "critical"
	clock.t = t0.Add(4 * time.Minute)
	c.guardianTick(ctx)
	require.Empty(t, fake.killed, "planned rotation hot-swaps the slot in place")
	require.Len(t, fake.spawned, 1, "planned rotation does not mint a new session")
	require.Len(t, fake.rotated, 1)
	require.Equal(t, "brain-1", fake.rotated[0].AgentID)
	require.Equal(t, RotateReasonContext, fake.rotated[0].Reason)
	require.Equal(t, "brain-1", c.runs[runID].brain.AgentID)

	// The freshly rotated brain is also critical, but the cooldown suppresses an
	// immediate re-rotation. Context is still keyed by the stable slot id.
	fake.activity[runID] = clock.t
	fake.ctxLevel["brain-1"] = "critical"
	c.guardianTick(ctx)
	require.Len(t, fake.rotated, 1, "cooldown prevents thrashing")

	// Past the cooldown, it rotates again — still the same slot.
	clock.t = t0.Add(20 * time.Minute)
	fake.activity[runID] = clock.t
	c.guardianTick(ctx)
	require.Len(t, fake.rotated, 2, "planned rotation resumes after the cooldown")
	require.Equal(t, "brain-1", fake.rotated[1].AgentID)
	require.Len(t, fake.spawned, 1)
}

// TestGuardianHonorsKillSwitch proves a disabled controller supervises nothing,
// and that a runtime without GuardianRuntime makes the tick a no-op.
func TestGuardianHonorsKillSwitch(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, _ := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	ctx := context.Background()

	c.Disable(ctx, "")
	clock.t = t0.Add(30 * time.Minute)
	c.guardianTick(ctx) // wedged by the clock, but disabled ⇒ no heal
	require.Empty(t, fake.nudges, "a disabled controller heals nothing")

	// A runtime that does not implement GuardianRuntime is never guardian-managed.
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	plainRT := newFakeRuntime()
	c2 := NewController(ControllerConfig{Plans: []string{plan}, BaseDir: dir, Resolver: cyclicResolver("a", "free"), Guardian: testGuardian()}, &fakeEnv{})
	c2.setClock(clock.now)
	c2.SetRuntime(plainRT)
	_, err := c2.Enable(ctx, "")
	require.NoError(t, err)
	clock.t = t0.Add(90 * time.Minute)
	c2.guardianTick(ctx) // must be a no-op — no GuardianRuntime
	require.Empty(t, plainRT.killed, "a non-guardian runtime is never healed")
}

// TestGuardianHealStagesHotSwap is the WP5 table: every heal-ladder stage that
// used to terminate+spawn now either nudges, hot-swaps the same slot, or backs
// off — the manager id never changes and TerminateBrain is never used to rotate.
func TestGuardianHealStagesHotSwap(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	type want struct {
		nudges  int
		rotated int
		backend string
		reason  string
		killed  int
		spawned int
		stage   healStage
		id      string
		backoff bool
	}
	tests := []struct {
		name  string
		res   Resolver
		setup func(c *Controller, fake *guardianFake, runID string, clock *fakeClock)
		want  want
	}{
		{
			name: "nudge",
			res:  cyclicResolver("a", "free"),
			setup: func(c *Controller, fake *guardianFake, runID string, clock *fakeClock) {
				clock.t = t0.Add(11 * time.Minute)
				c.guardianTick(ctx)
			},
			want: want{nudges: 1, spawned: 1, stage: stageNudged, id: "brain-1"},
		},
		{
			name: "restart",
			res:  cyclicResolver("a", "free"),
			setup: func(c *Controller, fake *guardianFake, runID string, clock *fakeClock) {
				c.runs[runID].healStage = stageNudged
				c.runs[runID].healNextAt = time.Time{}
				clock.t = t0.Add(11 * time.Minute)
				c.guardianTick(ctx)
			},
			want: want{rotated: 1, backend: "a", reason: RotateReasonHeal, spawned: 1, stage: stageRestarted, id: "brain-1"},
		},
		{
			name: "rotate",
			res:  &roundRobinResolver{backends: []string{"a", "b"}, tiers: []backendstore.ModelTier{"free", "subscription"}},
			setup: func(c *Controller, fake *guardianFake, runID string, clock *fakeClock) {
				c.runs[runID].healStage = stageRestarted
				c.runs[runID].healNextAt = time.Time{}
				c.runs[runID].tried = map[string]bool{"a": true}
				clock.t = t0.Add(11 * time.Minute)
				c.guardianTick(ctx)
			},
			want: want{rotated: 1, backend: "b", reason: RotateReasonHeal, spawned: 1, stage: stageRotated, id: "brain-1"},
		},
		{
			name: "backoff",
			res:  &fakeResolver{err: router.ErrAllExhausted},
			setup: func(c *Controller, fake *guardianFake, runID string, clock *fakeClock) {
				c.runs[runID].healStage = stageRotated
				c.runs[runID].healNextAt = time.Time{}
				clock.t = t0.Add(11 * time.Minute)
				c.guardianTick(ctx)
			},
			want: want{spawned: 1, stage: stageBackoff, id: "brain-1", backoff: true},
		},
		{
			name: "plannedRotate",
			res:  cyclicResolver("a", "free"),
			setup: func(c *Controller, fake *guardianFake, runID string, clock *fakeClock) {
				fake.activity[runID] = t0.Add(3 * time.Minute)
				fake.ctxLevel["brain-1"] = "critical"
				clock.t = t0.Add(4 * time.Minute)
				c.guardianTick(ctx)
			},
			want: want{rotated: 1, backend: "a", reason: RotateReasonContext, spawned: 1, stage: stageHealthy, id: "brain-1"},
		},
		{
			name: "missing-brain-respawns",
			res:  cyclicResolver("a", "free"),
			setup: func(c *Controller, fake *guardianFake, runID string, clock *fakeClock) {
				c.runs[runID].brain = nil
				c.runs[runID].healStage = stageRestarted
				c.runs[runID].healNextAt = time.Time{}
				clock.t = t0.Add(11 * time.Minute)
				c.guardianTick(ctx)
			},
			want: want{spawned: 2, rotated: 0, stage: stageRotated, id: "brain-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{t: t0}
			fake := newGuardianFake()
			c, runID := enabledGuardianController(t, fake, clock, tt.res, testGuardian())
			require.Equal(t, "brain-1", c.runs[runID].brain.AgentID)

			tt.setup(c, fake, runID, clock)

			require.Len(t, fake.nudges, tt.want.nudges)
			require.Len(t, fake.rotated, tt.want.rotated)
			require.Len(t, fake.killed, tt.want.killed)
			require.Len(t, fake.spawned, tt.want.spawned)
			require.Equal(t, tt.want.stage, c.runs[runID].healStage)
			require.Equal(t, tt.want.id, c.runs[runID].brain.AgentID, "manager slot id after %s", tt.name)
			if tt.want.rotated > 0 {
				last := fake.rotated[len(fake.rotated)-1]
				require.Equal(t, "brain-1", last.AgentID, "HotSwap target is the existing slot")
				require.Equal(t, tt.want.backend, last.Backend)
				require.Equal(t, tt.want.reason, last.Reason)
				require.NotEmpty(t, last.Prompt, "successor is seeded with the recovery digest")
			}
			if tt.want.backoff {
				require.NotNil(t, c.Status().Runs[0].Backoff)
			} else {
				require.Nil(t, c.Status().Runs[0].Backoff)
			}
		})
	}
}

// TestGuardianRotationLeavesLiveWorkersUntouched proves a HotSwap rotation does
// not rewrite worker parent_ids — the tree stays put and land ownership (tags)
// is unchanged. The daemon integration test covers the store/handoff/land path;
// this locks the controller seam: RotateBrain is the only mutation.
func TestGuardianRotationLeavesLiveWorkersUntouched(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	mgrID := c.runs[runID].brain.AgentID

	// Live workers parented at the manager slot (the pre-WP6 parent_id tree).
	type worker struct{ id, parent, branch string }
	workers := []worker{
		{id: "agent-w1", parent: mgrID, branch: "autopilot/task-a"},
		{id: "agent-w2", parent: mgrID, branch: "autopilot/task-b"},
	}
	for _, w := range workers {
		fake.roster[runID] = append(fake.roster[runID], AgentInfo{
			ID: w.id, Role: "worker", State: "working", Branch: w.branch,
			Tags: []string{autopilotTag, runTag(runID)},
		})
	}
	parentBefore := map[string]string{}
	for _, w := range workers {
		parentBefore[w.id] = w.parent
	}

	c.runs[runID].healStage = stageNudged
	c.runs[runID].healNextAt = time.Time{}
	clock.t = t0.Add(11 * time.Minute)
	c.guardianTick(context.Background())

	require.Len(t, fake.rotated, 1)
	require.Equal(t, mgrID, fake.rotated[0].AgentID)
	require.Equal(t, mgrID, c.runs[runID].brain.AgentID, "manager id unchanged")
	require.Empty(t, fake.killed)
	require.Len(t, fake.spawned, 1)

	// Roster (and therefore land-by-tag) is untouched; parent_id is not a
	// controller field — the fake workers still name the same parent.
	require.Len(t, fake.roster[runID], 2)
	for _, w := range workers {
		require.Equal(t, mgrID, parentBefore[w.id], "worker %s parent_id must not be rewritten", w.id)
	}
	owned, gotRun := ownershipFromTestTags([]string{autopilotTag, runTag(runID)})
	require.True(t, owned)
	require.Equal(t, runID, gotRun)
}

// ownershipFromTestTags mirrors daemon ownershipFromTags so the controller test
// can assert land-by-tag still holds without importing daemon.
func ownershipFromTestTags(tags []string) (owned bool, runID string) {
	has := false
	for _, t := range tags {
		switch {
		case t == autopilotTag:
			has = true
		case len(t) > 4 && t[:4] == "run:":
			runID = t[4:]
		}
	}
	return has && runID != "", runID
}
