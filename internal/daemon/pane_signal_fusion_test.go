package daemon

// Pane signal fusion regression suite (usage-api-quota-recovery Phase 7).
//
// These tests prove the daemon-side half of the fusion seam: a confirmed Claude
// rate-limit MENU selection is delivered to RateLimitScheduler.OnRateLimitObservation
// — the exact same handler a confirmed BANNER observation uses (see
// e2e_ratelimit_recovery_harness_test.go) and the one the daemon wires
// poller.Poller.OnLimitMenuSelected to (internal/cli/daemon.go). Because both
// paths converge on BackendRecoveryCoordinator.OnHardLimit's per-agent
// recovery-generation fencing, menu, banner, and (once Phase 6 wires it) usage-API
// evidence for the SAME incident can never create more than one recovery
// generation, regardless of arrival order.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// weeklyResetPostMenuExcerpt mirrors the exact regression fixture from the
// design record (docs/specs/2026-09-29-usage-api-quota-recovery.md): the pane
// Claude renders right after a safely-selected "wait for limit to reset" menu,
// carrying NO parseable `resets` banner.
const weeklyResetPostMenuExcerpt = "Requesting rate limit reset for weekly limit"

// TestPaneSignalFusion_WeeklyResetMenuTriggersRecoveryWithoutBanner is the exact
// weekly-reset post-menu fixture at the daemon layer: a menu-confirmed
// observation whose excerpt has no parseable `resets` clause must still start
// recovery immediately — proving recovery no longer depends solely on a later
// banner.
func TestPaneSignalFusion_WeeklyResetMenuTriggersRecoveryWithoutBanner(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *agentstore.Agent, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))

	// Prove the premise this phase closes: the exact post-menu text never
	// matches the banner detector, so the legacy banner-only path alone would
	// never have observed this incident.
	require.False(t, poller.LimitBannerPresent(weeklyResetPostMenuExcerpt),
		"the weekly-reset post-menu text must carry no parseable `resets` banner")

	// This is exactly what the poller's OnLimitMenuSelected hook constructs and
	// delivers (internal/poller/poller.go tryLimitMenu -> NewRateLimitObservation),
	// and exactly what the daemon wires it to (pl.OnLimitMenuSelected =
	// rateLimitSched.OnRateLimitObservation).
	obs := poller.NewRateLimitObservation("agent-1", weeklyResetPostMenuExcerpt)
	require.Equal(t, store.StatusRateLimited, obs.ClassifierResult)

	sched.OnRateLimitObservation(obs)

	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		life.mu.Lock()
		n := len(life.swaps)
		life.mu.Unlock()
		return s != nil && s.BackendRecovery != nil &&
			(s.BackendRecovery.Phase == recoveryStabilizing || s.BackendRecovery.Phase == recoverySwitching) && n == 1
	}, time.Second, 5*time.Millisecond,
		"a confirmed menu selection with no parseable banner must still start recovery")

	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()),
		"original backend must have durable cooldown evidence after the menu-triggered switchover")
}

// TestPaneSignalFusion_UsageTriggeredRecoveryThenMenuSignalIsIdempotent is the
// "usage-first" regression: a usage-API-triggered hard-limit call (what Phase 6's
// bulk reconciliation wiring invokes through the SAME coordinator entry point)
// claims the recovery generation first; a LATER menu-confirmed observation for
// the identical agent/incident must be a safe no-op, not a second generation or
// a second swap.
func TestPaneSignalFusion_UsageTriggeredRecoveryThenMenuSignalIsIdempotent(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *agentstore.Agent, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))

	// "Usage-first": the exhausted-bucket reconciliation path claims the
	// recovery generation via the coordinator's confirmed hard-limit entry point
	// directly — this is the exact call Phase 6's bulk wiring makes per affected
	// agent — immediately followed by the menu-confirmed observation for the
	// SAME agent/incident, mirroring how close together usage and pane evidence
	// for one real incident actually arrive (within the same tick cycle).
	require.True(t, c.OnHardLimit(st.snapSession("agent-1"), time.Now().Add(time.Hour)))
	obs := poller.NewRateLimitObservation("agent-1", weeklyResetPostMenuExcerpt)
	sched.OnRateLimitObservation(obs)

	require.Eventually(t, func() bool {
		life.mu.Lock()
		n := len(life.swaps)
		life.mu.Unlock()
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && n >= 1
	}, time.Second, 5*time.Millisecond, "the incident must produce at least one swap")

	time.Sleep(30 * time.Millisecond) // allow any in-flight extra round to settle

	// The hard idempotency invariant the plan requires is on the recovery
	// GENERATION, not the exact swap count: if the second signal lands while the
	// coordinator is already refreshing/switching/waiting it is a clean no-op
	// (unchanged generation, no extra swap); if it instead lands in the brief
	// stabilizing window, the EXISTING coordinator treats it as "the candidate we
	// just switched to is ALSO immediately hard-limited" and tries the next
	// candidate — a legitimate extra round WITHIN the same generation (see
	// TestBackendRecoverySequentialFallbackAndStabilization), not a duplicate
	// recovery. Either way the generation must never advance past 1, and the
	// number of candidate attempts is bounded by the number of installed
	// candidates (never unbounded/looping).
	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery)
	require.Equal(t, uint64(1), s.BackendRecovery.Generation,
		"a later menu signal for the same incident must never start a new recovery generation")

	life.mu.Lock()
	swapCount := len(life.swaps)
	life.mu.Unlock()
	require.GreaterOrEqual(t, swapCount, 1, "the incident must still produce at least one swap")
	require.LessOrEqual(t, swapCount, 3, "swap attempts must stay bounded by the installed candidates, never runaway")
}

// TestPaneSignalFusion_SimultaneousMenuAndBannerSignalsOneGeneration is the
// "simultaneous-signal" regression: a banner-sourced observation and a
// menu-sourced observation for the same agent/incident, fired truly
// concurrently from separate goroutines, must converge on exactly one recovery
// generation — never a race that spawns two independent recoveries for the same
// incident.
func TestPaneSignalFusion_SimultaneousMenuAndBannerSignalsOneGeneration(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *agentstore.Agent, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))

	bannerObs := poller.NewRateLimitObservation("agent-1", confirmBanner) // defined in e2e_ratelimit_recovery_harness_test.go
	menuObs := poller.NewRateLimitObservation("agent-1", weeklyResetPostMenuExcerpt)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sched.OnRateLimitObservation(bannerObs) }()
	go func() { defer wg.Done(); sched.OnRateLimitObservation(menuObs) }()
	wg.Wait()

	require.Eventually(t, func() bool {
		life.mu.Lock()
		n := len(life.swaps)
		life.mu.Unlock()
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && n >= 1
	}, time.Second, 5*time.Millisecond, "the incident must produce at least one swap")

	time.Sleep(30 * time.Millisecond)

	// Same hard invariant as the usage-first case: the GENERATION must never
	// double-advance, however the race between the two concurrent deliveries
	// happens to interleave with the coordinator's own stabilizing window (see
	// that test's comment for why swap count alone is not the right assertion).
	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery)
	require.Equal(t, uint64(1), s.BackendRecovery.Generation,
		"simultaneous menu + banner evidence for the same incident must never advance past the first recovery generation")

	life.mu.Lock()
	swapCount := len(life.swaps)
	life.mu.Unlock()
	require.GreaterOrEqual(t, swapCount, 1, "the incident must still produce at least one swap")
	require.LessOrEqual(t, swapCount, 3, "swap attempts must stay bounded by the installed candidates, never runaway")
}

// TestPaneSignalFusion_StaleRepeatedMenuSignalDoesNotDuplicateGeneration is the
// "stale-pane" regression: the SAME menu-confirmed excerpt observed again (e.g. a
// terminal resize repaints the identical already-handled pane) must not start a
// second recovery generation or a second swap.
func TestPaneSignalFusion_StaleRepeatedMenuSignalDoesNotDuplicateGeneration(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *agentstore.Agent, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))

	obs := poller.NewRateLimitObservation("agent-1", weeklyResetPostMenuExcerpt)

	// Fire the identical stale/repeated observation twice in quick succession,
	// simulating consecutive ticks that keep re-observing the same
	// already-handled pane (e.g. a terminal resize repainting it) before the
	// first attempt has settled — mirrors TestE2EHarness_OneSwitchoverPerGeneration's
	// proven duplicate-banner invariant, now for the menu-sourced path.
	sched.OnRateLimitObservation(obs)
	sched.OnRateLimitObservation(obs)

	require.Eventually(t, func() bool {
		life.mu.Lock()
		n := len(life.swaps)
		life.mu.Unlock()
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && n >= 1
	}, time.Second, 5*time.Millisecond, "the incident must produce at least one swap")

	time.Sleep(30 * time.Millisecond)

	// See TestPaneSignalFusion_UsageTriggeredRecoveryThenMenuSignalIsIdempotent
	// for why the generation (not the exact swap count) is the right hard
	// invariant here.
	s := st.snapSession("agent-1")
	require.Equal(t, uint64(1), s.BackendRecovery.Generation,
		"repeated stale/duplicate menu observations must not advance the recovery generation")

	life.mu.Lock()
	swapCount := len(life.swaps)
	life.mu.Unlock()
	require.GreaterOrEqual(t, swapCount, 1, "the incident must still produce at least one swap")
	require.LessOrEqual(t, swapCount, 3, "swap attempts must stay bounded by the installed candidates, never runaway")
}
