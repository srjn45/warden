package autopilot

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func supervisorFixture(t *testing.T, r *run) (*Controller, Supervisor, *NextStep) {
	t.Helper()
	c := NewController(ControllerConfig{BaseDir: t.TempDir(), Resolver: &fakeResolver{backendID: "a", tier: "free"}}, &fakeEnv{})
	c.SetRuntime(newFakeRuntime())
	r.slotScope = "x"
	c.mu.Lock()
	defer c.mu.Unlock()
	return c, c.supervisorLocked(r), c.nextStepLocked(r, time.Now())
}

func TestSupervisorKindStateHealth(t *testing.T) {
	tick := time.Now()
	cases := []struct {
		name   string
		r      run
		kind   string
		state  string
		health string
		stage  string
		owner  string // "" = no next step
	}{
		{"registered", run{state: StateRegistered}, SupervisorNone, SupStateSuspended, HealthUnknown, "none", ""},
		{"paused", run{state: StatePaused}, SupervisorNone, SupStateSuspended, HealthUnknown, "none", ""},
		{"stopped", run{state: StateStopped}, SupervisorNone, SupStateSuspended, HealthUnknown, "none", ""},
		{"complete", run{state: StateComplete}, SupervisorNone, SupStateSuspended, HealthUnknown, "none", ""},
		{"disabled", run{state: StateDisabled}, SupervisorNone, SupStateDisabled, HealthUnknown, "none", ""},
		{"boot unknown", run{state: StateActive}, SupervisorDaemon, SupStateSupervising, HealthUnknown, "none", "daemon"},
		{"healthy", run{state: StateActive, lastTickAt: tick}, SupervisorDaemon, SupStateSupervising, HealthHealthy, "none", "daemon"},
		{"nudged", run{state: StateHealing, lastTickAt: tick, healStage: stageNudged}, SupervisorDaemon, SupStateSupervising, HealthHealing, "nudged", "daemon"},
		{"restarted", run{state: StateHealing, lastTickAt: tick, healStage: stageRestarted}, SupervisorDaemon, SupStateSupervising, HealthHealing, "restarted", "daemon"},
		{"rotated", run{state: StateHealing, lastTickAt: tick, healStage: stageRotated}, SupervisorDaemon, SupStateSupervising, HealthHealing, "rotated", "daemon"},
		{"backoff", run{state: StateDegraded, lastTickAt: tick, healStage: stageBackoff, backoffNextRetry: tick}, SupervisorDaemon, SupStateSupervising, HealthBackoff, "backoff", "daemon"},
		{"parked", run{state: StateDegraded, lastTickAt: tick, needsAttention: "fix it"}, SupervisorDaemon, SupStateSupervising, HealthParked, "none", "operator"},
		{"parked at boot", run{state: StateActive, needsAttention: "fix it"}, SupervisorDaemon, SupStateSupervising, HealthParked, "none", "operator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.r
			c, sv, ns := supervisorFixture(t, &r)
			require.Equal(t, tc.kind, sv.Kind)
			require.Equal(t, tc.state, sv.State)
			require.Equal(t, tc.health, sv.Health)
			require.Equal(t, tc.stage, sv.HealStage)
			require.False(t, sv.Persisted)
			// S1: nothing here is a session, so nothing is addressable.
			require.False(t, sv.Addressable)
			require.Empty(t, sv.SessionID)
			if tc.owner == "" {
				require.Nil(t, ns)
				return
			}
			require.Equal(t, tc.owner, ns.Owner)
			require.Empty(t, ns.AddressableSessionID)
			// S2: parked <=> needs_attention <=> operator owner.
			require.Equal(t, sv.Health == HealthParked, ns.Owner == "operator")
			require.Equal(t, r.needsAttention != "", ns.Owner == "operator")
			require.Empty(t, c.nextStepViolation(&r, time.Now()))
		})
	}
}

func TestSupervisorAwaitingMergeIsIdleDaemon(t *testing.T) {
	r := run{state: StateActive, lastTickAt: time.Now(), completion: completionState{awaiting: &AwaitingMerge{PR: 7}}}
	_, sv, ns := supervisorFixture(t, &r)
	require.Equal(t, SupervisorDaemon, sv.Kind)
	require.Equal(t, SupStateIdle, sv.State)
	require.Equal(t, "daemon", ns.Owner)
}

func TestNextStepOwnerNeverGuardian(t *testing.T) {
	for _, stage := range []healStage{stageHealthy, stageNudged, stageRestarted, stageRotated, stageBackoff} {
		r := run{state: StateHealing, healStage: stage, lastTickAt: time.Now(), backoffNextRetry: time.Now()}
		_, _, ns := supervisorFixture(t, &r)
		require.Equal(t, "daemon", ns.Owner, "stage %v", stage)
	}
}

func TestStatusDoesNotAdvertiseGuardianAsSession(t *testing.T) {
	r := run{runID: "ap-1", state: StateActive, lastTickAt: time.Now()}
	c, _, _ := supervisorFixture(t, &r)
	c.mu.Lock()
	rs := c.runStatusLocked(&r)
	c.mu.Unlock()
	raw, err := json.Marshal(rs)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.NotContains(t, m, "guardian_id") // S1/S4: phase 1 omits it
	require.Equal(t, GuardianSlotID("x"), m["guardian_slot_id"])
	sup := m["supervisor"].(map[string]any)
	require.Equal(t, false, sup["addressable"])
	require.NotContains(t, sup, "session_id")
	for k, v := range m {
		if k == "guardian_slot_id" {
			continue
		}
		if s, ok := v.(string); ok {
			require.NotEqual(t, GuardianSlotID("x"), s, k)
		}
	}
}

func TestGuardianTickStampsLastTickAndWalksHealth(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	c, _ := enabledGuardianController(t, fake, clock, &fakeResolver{backendID: "a", tier: "free"}, testGuardian())
	st := c.Status().Runs[0]
	require.Equal(t, HealthUnknown, st.Supervisor.Health, "no tick yet since boot")
	require.Empty(t, st.Supervisor.LastTickAt)

	clock.t = t0.Add(time.Minute)
	fake.activity = map[string]time.Time{st.RunID: clock.t}
	c.guardianTick(context.Background())
	st = c.Status().Runs[0]
	require.Equal(t, HealthHealthy, st.Supervisor.Health)
	require.Equal(t, clock.t.UTC().Format(time.RFC3339), st.Supervisor.LastTickAt)

	clock.t = t0.Add(20 * time.Minute) // stale heartbeat -> nudge
	c.guardianTick(context.Background())
	st = c.Status().Runs[0]
	require.Equal(t, HealthHealing, st.Supervisor.Health)
	require.Equal(t, "nudged", st.Supervisor.HealStage)
	require.Equal(t, "daemon", st.NextStep.Owner)
}
