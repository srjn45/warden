package autopilot

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

// Supervision-recovery validation (truthful-supervisor-status contract §5.3):
// characterisation of the daemon supervisor through recovery, restart and legacy
// data, plus the S1/S4 negative invariants.

// staleGuardianRef matches text that would direct a reader to contact a guardian.
var staleGuardianRef = regexp.MustCompile(`(?i)(message|ask|tell|contact|attach|get_agent|send_message)[^.\n]{0,40}guardian|-guardian\b`)

func TestSupervisorHealLadderTransitions(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	res := &roundRobinResolver{
		backends: []string{"a", "b"},
		tiers:    []backendstore.ModelTier{"free", "subscription"},
	}
	c, runID := enabledGuardianController(t, fake, clock, res, testGuardian())
	ctx := context.Background()

	// 1. Initial healthy state after first activity tick
	clock.t = t0.Add(5 * time.Minute)
	guardianTickAndWait(t, c, ctx)
	st := c.Status().Runs[0]
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind)
	require.False(t, st.Supervisor.Addressable)
	require.Empty(t, st.Supervisor.SessionID)
	require.Equal(t, HealthHealthy, st.Supervisor.Health)
	require.Equal(t, "none", st.Supervisor.HealStage)
	require.Equal(t, "daemon", st.NextStep.Owner)
	require.Equal(t, "supervise: landing, overwatch and stall triage", st.NextStep.Action)

	// 2. Stage 1: Nudged (past heartbeat timeout)
	clock.t = t0.Add(11 * time.Minute)
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind)
	require.False(t, st.Supervisor.Addressable)
	require.Equal(t, HealthHealing, st.Supervisor.Health)
	require.Equal(t, "nudged", st.Supervisor.HealStage)
	require.Equal(t, "daemon", st.NextStep.Owner)
	require.Equal(t, "restart the manager if it shows no heartbeat", st.NextStep.Action)
	require.NotEmpty(t, st.NextStep.At)

	// 3. Stage 2: Restarted (grace elapsed, still wedged)
	clock.t = t0.Add(22 * time.Minute)
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind)
	require.False(t, st.Supervisor.Addressable)
	require.Equal(t, HealthHealing, st.Supervisor.Health)
	require.Equal(t, "restarted", st.Supervisor.HealStage)
	require.Equal(t, "daemon", st.NextStep.Owner)
	require.Equal(t, "rotate the manager to the next backend if it shows no heartbeat", st.NextStep.Action)
	require.NotEmpty(t, st.NextStep.At)

	// 4. Stage 3: Rotated (grace elapsed, still wedged)
	clock.t = t0.Add(33 * time.Minute)
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind)
	require.False(t, st.Supervisor.Addressable)
	require.Equal(t, HealthHealing, st.Supervisor.Health)
	require.Equal(t, "rotated", st.Supervisor.HealStage)
	require.Equal(t, "daemon", st.NextStep.Owner)
	require.Equal(t, "back off (or park) if the rotated manager shows no heartbeat", st.NextStep.Action)
	require.NotEmpty(t, st.NextStep.At)

	// 5. Stage 4: Backoff (ladder exhausted)
	clock.t = t0.Add(44 * time.Minute)
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind)
	require.False(t, st.Supervisor.Addressable)
	require.Equal(t, HealthBackoff, st.Supervisor.Health)
	require.Equal(t, "backoff", st.Supervisor.HealStage)
	require.Equal(t, "daemon", st.NextStep.Owner)
	require.Equal(t, "retry the backend ladder from the top", st.NextStep.Action)
	require.NotEmpty(t, st.NextStep.At)
	require.NotNil(t, st.Backoff)

	// 6. Recovers on heartbeat
	retry, err := time.Parse(time.RFC3339, st.Backoff.NextRetryAt)
	require.NoError(t, err)
	clock.t = retry.Add(time.Second)
	fake.activity[runID] = clock.t
	guardianTickAndWait(t, c, ctx)
	clock.t = clock.t.Add(time.Minute)
	fake.activity[runID] = clock.t
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.Equal(t, HealthHealthy, st.Supervisor.Health)
	require.Equal(t, "none", st.Supervisor.HealStage)
	require.Nil(t, st.Backoff)
	require.Equal(t, "daemon", st.NextStep.Owner)
}

func TestSupervisorLimitRestingAndAwaitingMerge(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	// Limit resting:
	r := run{
		runID: "ap-resting", state: StateActive, lastTickAt: now,
		brain: &BrainHandle{AgentID: "brain-1"},
		resting: map[string]*restingAgent{
			"brain-1": {until: now.Add(time.Hour)},
		},
	}
	_, sv, ns := supervisorFixture(t, &r)
	require.Equal(t, SupervisorDaemon, sv.Kind)
	require.False(t, sv.Addressable)
	require.Equal(t, HealthHealthy, sv.Health)
	require.NotNil(t, ns)
	require.Equal(t, "daemon", ns.Owner)
	require.Equal(t, "resume or switch a rate-limited agent", ns.Action)
	require.Equal(t, now.Add(time.Hour).Format(time.RFC3339), ns.At)

	// Awaiting merge:
	rMerge := run{
		runID: "ap-awaiting", state: StateActive, lastTickAt: now,
		completion: completionState{awaiting: &AwaitingMerge{PR: 42, nextPollAt: now.Add(2 * time.Minute)}},
	}
	_, svMerge, nsMerge := supervisorFixture(t, &rMerge)
	require.Equal(t, SupervisorDaemon, svMerge.Kind)
	require.False(t, svMerge.Addressable)
	require.Equal(t, SupStateIdle, svMerge.State)
	require.Equal(t, HealthHealthy, svMerge.Health)
	require.NotNil(t, nsMerge)
	require.Equal(t, "daemon", nsMerge.Owner)
	require.Equal(t, "check final PR #42 for merge", nsMerge.Action)
	require.Equal(t, now.Add(2*time.Minute).Format(time.RFC3339), nsMerge.At)
}

func TestSupervisorMissingManagerHealsToHealthy(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	ctx := context.Background()

	clock.t = t0.Add(time.Minute)
	fake.activity = map[string]time.Time{runID: clock.t}
	guardianTickAndWait(t, c, ctx)
	require.Equal(t, HealthHealthy, c.Status().Runs[0].Supervisor.Health)

	// The manager is killed out from under the run.
	fake.missing["brain-1"] = true
	fake.spawnErrOn["a"] = context.DeadlineExceeded
	clock.t = t0.Add(2 * time.Minute)
	guardianTickAndWait(t, c, ctx)
	st := c.Status().Runs[0]
	require.NotEqual(t, HealthHealthy, st.Supervisor.Health, "no manager is never reported healthy")
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind, "supervisor stays the daemon while the manager is gone")
	require.Nil(t, st.Brain)
	require.Equal(t, "daemon", st.NextStep.Owner)

	// Backend recovers; the next retry respawns the manager and health converges.
	delete(fake.spawnErrOn, "a")
	require.NotNil(t, st.Backoff)
	retry, err := time.Parse(time.RFC3339, st.Backoff.NextRetryAt)
	require.NoError(t, err)
	clock.t = retry.Add(time.Second)
	fake.activity[runID] = clock.t
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.NotNil(t, st.Brain)
	require.Equal(t, "brain-2", st.Brain.AgentID)
	clock.t = clock.t.Add(time.Minute)
	fake.activity[runID] = clock.t
	guardianTickAndWait(t, c, ctx)
	st = c.Status().Runs[0]
	require.Equal(t, HealthHealthy, st.Supervisor.Health)
	require.Equal(t, "none", st.Supervisor.HealStage)
	require.Nil(t, st.Backoff)
}

func TestSupervisorRestartRebuildsUnknownThenConverges(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	clock.t = t0.Add(time.Minute)
	fake.activity = map[string]time.Time{runID: clock.t}
	guardianTickAndWait(t, c, context.Background())
	require.Equal(t, HealthHealthy, c.Status().Runs[0].Supervisor.Health)

	// Daemon restart: heal/tick state is in-memory only.
	c.mu.Lock()
	r := c.runs[runID]
	r.lastTickAt, r.healStage = time.Time{}, stageHealthy
	c.mu.Unlock()
	st := c.Status().Runs[0]
	require.Equal(t, HealthUnknown, st.Supervisor.Health)
	require.Empty(t, st.Supervisor.LastTickAt)
	require.False(t, st.Supervisor.Persisted)

	clock.t = clock.t.Add(time.Minute)
	fake.activity[runID] = clock.t
	guardianTickAndWait(t, c, context.Background())
	require.Equal(t, HealthHealthy, c.Status().Runs[0].Supervisor.Health, "converges after one tick")
}

func TestSupervisorAwaitingMergeSurvivesRestart(t *testing.T) {
	c, _, id, now := awaitingSetup(t)
	c.mu.Lock()
	r := c.runs[id]
	r.completion = completionState{}
	r.surface, r.surfaceLoaded = surfaceRecord{}, false
	r.lastTickAt = time.Time{}
	c.mu.Unlock()

	st, err := c.LookupRun(id)
	require.NoError(t, err)
	require.NotNil(t, st.AwaitingMerge, "awaiting_merge is durable")
	require.Equal(t, SupervisorDaemon, st.Supervisor.Kind)
	require.Equal(t, SupStateIdle, st.Supervisor.State)
	require.Equal(t, HealthUnknown, st.Supervisor.Health, "heal state is not claimed after restart")
	require.Equal(t, "daemon", st.NextStep.Owner)
	require.Empty(t, st.NextStep.AddressableSessionID)
	_ = now
}

func TestSupervisorParkedIsOperatorOwnedAndSurvivesTicks(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, runID := enabledGuardianController(t, fake, clock, cyclicResolver("a", "free"), testGuardian())
	c.mu.Lock()
	c.runs[runID].needsAttention = "plan is broken"
	c.mu.Unlock()
	for i := 1; i <= 3; i++ {
		clock.t = t0.Add(time.Duration(i) * 20 * time.Minute)
		guardianTickAndWait(t, c, context.Background())
		st := c.Status().Runs[0]
		require.Equal(t, HealthParked, st.Supervisor.Health)
		require.Equal(t, "operator", st.NextStep.Owner)
		require.Equal(t, "plan is broken", st.NeedsAttention)
	}
	require.Empty(t, fake.nudges, "a parked run is not nudged")
}

func TestLegacyGuardianRecordIsNormalized(t *testing.T) {
	// A pre-drop run record carried guardian_id; it must decode and never re-emit.
	raw := `{"run_id":"ap-old","name":"old","repo":"/r","plan_file":"/r/p.yaml","state":"active","guardian_id":"old-guardian","slot_scope":"old"}`
	var rec RunRecord
	require.NoError(t, json.Unmarshal([]byte(raw), &rec))
	require.Equal(t, "old", rec.SlotScope)
	out, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NotContains(t, string(out), "guardian_id")

	r := run{runID: "ap-old", state: StateActive, slotScope: "old", lastTickAt: time.Now()}
	c := NewController(ControllerConfig{BaseDir: t.TempDir(), Resolver: &fakeResolver{backendID: "a", tier: "free"}}, &fakeEnv{})
	setRuntimeAndWait(t, c, newFakeRuntime())
	c.mu.Lock()
	rs := c.runStatusLocked(&r)
	c.mu.Unlock()
	js, err := json.Marshal(rs)
	require.NoError(t, err)
	require.NotContains(t, string(js), `"guardian_id"`)
	require.False(t, rs.Supervisor.Addressable)
	// The reserved name stays reserved so a stale id can never become a real agent.
	require.Error(t, validatePlanNameReservedSuffixes("old-guardian"))
}

// TestNoNonAddressableIDDirectsContact is the S4 negative: status JSON, next-step
// text, the nudge, the digest and the manager role prompt never tell anyone to
// contact the (non-session) supervisor.
func TestNoNonAddressableIDDirectsContact(t *testing.T) {
	slot := GuardianSlotID("x")

	for _, st := range []run{
		{state: StateActive}, {state: StateActive, lastTickAt: time.Now()},
		{state: StateHealing, healStage: stageNudged, lastTickAt: time.Now()},
		{state: StateHealing, healStage: stageRotated, lastTickAt: time.Now()},
		{state: StateDegraded, healStage: stageBackoff, lastTickAt: time.Now(), backoffNextRetry: time.Now()},
		{state: StateDegraded, needsAttention: "fix it", lastTickAt: time.Now()},
		{state: StatePaused}, {state: StateStopped}, {state: StateComplete},
	} {
		r := st
		r.runID = "ap-1"
		c, sv, ns := supervisorFixture(t, &r)
		c.mu.Lock()
		rs := c.runStatusLocked(&r)
		c.mu.Unlock()
		raw, err := json.Marshal(rs)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		delete(m, "guardian_slot_id") // the reservation marker is the one allowed carrier
		require.NotContains(t, string(mustJSON(t, m)), slot, "state %s", r.state)
		require.False(t, sv.Addressable)
		if ns != nil {
			require.NotContains(t, []string{"guardian", "overwatch"}, ns.Owner)
			require.Empty(t, ns.AddressableSessionID)
			require.False(t, staleGuardianRef.MatchString(ns.Action), ns.Action)
		}
	}

	require.False(t, staleGuardianRef.MatchString(guardianNudge), guardianNudge)

	out, err := ComposeDigest(context.Background(), DigestInput{
		RunID: "ap-run", Repo: "/repo", PlanFile: "/repo/plan.yaml",
		Plan: Plan{Version: 1, Goal: "ship it"}, IntegrationBranch: "autopilot/ship",
	})
	require.NoError(t, err)
	require.False(t, staleGuardianRef.MatchString(out), "digest must not name a guardian to contact")

	prompt, err := os.ReadFile("../role/roles/autopilot.yaml")
	require.NoError(t, err)
	for _, line := range strings.Split(string(prompt), "\n") {
		require.False(t, staleGuardianRef.MatchString(line), line)
	}
}

// TestSupervisorSessionKindSatisfiesS1 guards the reserved kind=session variant:
// a supervisor carrying a live agent id is the only addressable shape.
func TestSupervisorSessionKindSatisfiesS1(t *testing.T) {
	live := map[string]bool{"agent-42": true}
	satisfiesS1 := func(s Supervisor) bool {
		return s.Addressable == (s.Kind == SupervisorSession && s.SessionID != "" && live[s.SessionID])
	}
	sess := Supervisor{Kind: SupervisorSession, Addressable: true, SessionID: "agent-42", State: SupStateSupervising, Health: HealthHealthy, HealStage: "none"}
	require.True(t, satisfiesS1(sess))
	raw := mustJSON(t, sess)
	var back Supervisor
	require.NoError(t, json.Unmarshal(raw, &back))
	require.Equal(t, sess, back)
	require.Contains(t, string(raw), `"addressable":true`)
	require.Contains(t, string(raw), `"session_id":"agent-42"`)

	// Every shape the daemon can emit today is non-addressable and S1-valid.
	r := run{state: StateActive, lastTickAt: time.Now()}
	_, sv, _ := supervisorFixture(t, &r)
	require.True(t, satisfiesS1(sv))
	// Session-shaped id on a non-session kind, or a dead id, violates S1.
	require.False(t, satisfiesS1(Supervisor{Kind: SupervisorDaemon, Addressable: true, SessionID: "agent-42"}))
	require.False(t, satisfiesS1(Supervisor{Kind: SupervisorSession, Addressable: true, SessionID: "gone"}))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
