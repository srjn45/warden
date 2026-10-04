package daemon

// Phase 10 acceptance gate for docs/specs/2026-09-29-usage-api-quota-recovery.md.
//
// Reproduces the motivating incident end-to-end against the real daemon wiring
// used by Phases 5–9: two bound Claude agents share a weekly capacity domain,
// both hit Claude's wait-for-reset menu with no parseable `resets` banner, the
// usage API reports the shared weekly bucket exhausted, reconciliation selects
// both bound agents (never an unbound legacy peer), and recovery starts once
// per agent through BackendRecoveryCoordinator — simultaneous menu + usage
// evidence cannot create duplicate generations.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestUsageAPIQuotaRecovery_Phase10Acceptance is the upgrade-level gate for
// plan-69eb481d Phase 10 (migration-docs-acceptance).
func TestUsageAPIQuotaRecovery_Phase10Acceptance(t *testing.T) {
	fp := "sha256:shared-claude-weekly"
	s, coord, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		// Codex has headroom so both agents can recover without operator action.
		"codex":       {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(40)}},
	})

	// 1–2. Two Claude agents share the same account/profile capacity domain and
	// weekly mandatory bucket (daemon-owned QuotaBinding).
	boundAgent(t, st, "claude-a", "claude", "claude-model", fp, "weekly")
	boundAgent(t, st, "claude-b", "claude", "claude-model", fp, "weekly")

	// Compatibility: an older peer with only backend/model (unbound_legacy) must
	// stay operable and must NEVER be mass-swapped from guessed account/bucket.
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "claude-legacy", AiCli: "claude", Model: "claude-model",
		Role: "general", Status: store.StatusWorking,
	}))
	legacy, err := st.Get(context.Background(), "claude-legacy")
	require.NoError(t, err)
	require.Nil(t, legacy.QuotaBinding)
	require.Equal(t, capacity.LegacyUnbound, legacy.CapacityBindingState())

	// 3–5. Both bound agents encounter Claude's limit menu; the daemon selects
	// wait-for-reset; the post-menu pane carries NO parseable `resets` banner.
	require.False(t, poller.LimitBannerPresent(weeklyResetPostMenuExcerpt),
		"acceptance premise: weekly-reset post-menu text has no parseable resets banner")

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *agentstore.Agent, until time.Time) bool {
		return coord.OnHardLimit(sess, until)
	}
	for _, id := range []string{"claude-a", "claude-b"} {
		require.NoError(t, st.UpdateStatus(context.Background(), id, store.StatusRateLimited))
		obs := poller.NewRateLimitObservation(id, weeklyResetPostMenuExcerpt)
		require.Equal(t, store.StatusRateLimited, obs.ClassifierResult)
		sched.OnRateLimitObservation(obs)
	}

	// Menu evidence alone starts per-agent recovery (Phase 7 fusion). Wait until
	// both generations are claimed before the usage-API pass so we can prove
	// simultaneous dual-source evidence is generation-idempotent.
	for _, id := range []string{"claude-a", "claude-b"} {
		require.Eventually(t, func() bool {
			sess := st.snapSession(id)
			return sess != nil && sess.BackendRecovery != nil && sess.BackendRecovery.Generation == 1
		}, time.Second, 5*time.Millisecond, "menu-confirmed weekly limit must start recovery for %s without a banner", id)
	}

	// 6. Fresh successful Claude usage snapshot reports the shared weekly bucket
	// exhausted — authoritative capacity signal for every bound agent on that domain.
	recordExhaustedBucket(t, snapStore, "claude", fp, "weekly", nil)

	// 7. Reconciliation identifies both bound agents and skips the unbound legacy peer.
	impact, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, impact.ExhaustedBuckets, 1)
	require.Equal(t, "weekly", impact.ExhaustedBuckets[0].BucketKey)
	require.Equal(t, capacity.SourceUsage, impact.ExhaustedBuckets[0].Source)

	// Menu already claimed generation 1, so usage reconciliation must treat both
	// as already_reconciled / recovering — never as a second mass-swap wave.
	require.Empty(t, impact.AffectedAgents,
		"usage evidence for an already-recovering generation must not re-select agents")
	require.Equal(t, capacity.SkipUnboundLegacy, skipReason(impact, "claude-legacy"),
		"unbound_legacy must never be mass-swapped from a guessed account/bucket")

	// 8–9. Simultaneous late usage + repeated menu evidence must not advance past
	// generation 1. Fire concurrent dual-source signals for both agents.
	var wg sync.WaitGroup
	for _, id := range []string{"claude-a", "claude-b"} {
		id := id
		wg.Add(2)
		go func() {
			defer wg.Done()
			sched.OnRateLimitObservation(poller.NewRateLimitObservation(id, weeklyResetPostMenuExcerpt))
		}()
		go func() {
			defer wg.Done()
			_ = coord.OnHardLimit(st.snapSession(id), time.Now().Add(time.Hour))
		}()
	}
	wg.Wait()
	time.Sleep(30 * time.Millisecond)

	for _, id := range []string{"claude-a", "claude-b"} {
		sess := st.snapSession(id)
		require.NotNil(t, sess.BackendRecovery)
		require.Equal(t, uint64(1), sess.BackendRecovery.Generation,
			"%s must keep a single recovery generation across menu+usage evidence", id)
		require.Eventually(t, func() bool {
			s := st.snapSession(id)
			return s != nil && s.BackendRecovery != nil &&
				(s.BackendRecovery.Phase == recoveryStabilizing || s.BackendRecovery.Phase == recoverySwitching ||
					s.BackendRecovery.Phase == recoveryWaiting) &&
				s.AiCli != "claude"
		}, 2*time.Second, 5*time.Millisecond,
			"%s must leave the exhausted Claude pool without operator intervention", id)
	}

	// Legacy peer remains untouched — still unbound, still no recovery record.
	legacy = st.snapSession("claude-legacy")
	require.NotNil(t, legacy)
	require.Nil(t, legacy.QuotaBinding)
	require.Nil(t, legacy.BackendRecovery)
	require.Equal(t, "claude", legacy.AiCli)
	require.Equal(t, capacity.LegacyUnbound, legacy.CapacityBindingState())

	life.mu.Lock()
	swapCount := len(life.swaps)
	life.mu.Unlock()
	require.GreaterOrEqual(t, swapCount, 2, "each bound agent must get at least one recovery swap")
	require.LessOrEqual(t, swapCount, 6, "swap attempts must stay bounded; no runaway dual-source loops")
}

// TestUsageAPIQuotaRecovery_Phase10UsageFirstBothAgents covers the arrival-order
// twin of the acceptance gate: usage API claims both bound Claude agents first,
// then menu evidence for the same incident is generation-idempotent — both still
// recover without user intervention.
func TestUsageAPIQuotaRecovery_Phase10UsageFirstBothAgents(t *testing.T) {
	fp := "sha256:shared-claude-usage-first"
	s, coord, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(15)}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(50)}},
	})
	boundAgent(t, st, "claude-a", "claude", "claude-model", fp, "weekly")
	boundAgent(t, st, "claude-b", "claude", "claude-model", fp, "weekly")
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "claude-legacy", AiCli: "claude", Model: "claude-model",
		Role: "general", Status: store.StatusWorking,
	}))

	recordExhaustedBucket(t, snapStore, "claude", fp, "weekly", nil)
	impact, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"claude-a", "claude-b"}, impactIDs(impact))
	require.Equal(t, capacity.SkipUnboundLegacy, skipReason(impact, "claude-legacy"))

	s.startBulkRecovery(context.Background(), impact.AffectedAgents)

	for _, id := range []string{"claude-a", "claude-b"} {
		require.Eventually(t, func() bool {
			sess := st.snapSession(id)
			return sess != nil && sess.BackendRecovery != nil && sess.BackendRecovery.Generation == 1
		}, time.Second, 5*time.Millisecond, "%s must start recovery from usage impact", id)
	}

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *agentstore.Agent, until time.Time) bool {
		return coord.OnHardLimit(sess, until)
	}
	for _, id := range []string{"claude-a", "claude-b"} {
		sched.OnRateLimitObservation(poller.NewRateLimitObservation(id, weeklyResetPostMenuExcerpt))
	}
	time.Sleep(30 * time.Millisecond)

	for _, id := range []string{"claude-a", "claude-b"} {
		sess := st.snapSession(id)
		require.Equal(t, uint64(1), sess.BackendRecovery.Generation,
			"later menu evidence must not start a second generation for %s", id)
		require.Eventually(t, func() bool {
			s := st.snapSession(id)
			return s != nil && s.BackendRecovery != nil &&
				(s.BackendRecovery.Phase == recoveryStabilizing || s.BackendRecovery.Phase == recoverySwitching ||
					s.BackendRecovery.Phase == recoveryWaiting) &&
				s.AiCli != "claude"
		}, 2*time.Second, 5*time.Millisecond,
			"%s must leave the exhausted Claude pool without operator intervention", id)
	}

	legacy := st.snapSession("claude-legacy")
	require.Nil(t, legacy.BackendRecovery)
	require.Equal(t, capacity.LegacyUnbound, legacy.CapacityBindingState())

	life.mu.Lock()
	defer life.mu.Unlock()
	require.GreaterOrEqual(t, len(life.swaps), 2)
}
