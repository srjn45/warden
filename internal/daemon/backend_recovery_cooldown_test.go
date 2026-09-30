package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// recoveryFixtureWithDir is like recoveryFixture but uses a caller-supplied
// directory so tests can reopen the same store after simulating a daemon restart.
func recoveryFixtureWithDir(t *testing.T, dir string, limits map[string][]backendusage.Limit) (*BackendRecoveryCoordinator, *fakeStore, *recoveryLife) {
	t.Helper()
	bs, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	seeded, err := bs.ListModels("")
	require.NoError(t, err)
	for _, model := range seeded {
		require.NoError(t, bs.SetModelEnabled(model.BackendID, model.ModelID, false))
	}
	var adapters []backendusage.Adapter
	for _, id := range []string{"codex", "claude", "antigravity"} {
		require.NoError(t, bs.Upsert(backendstore.Backend{ID: id, Installed: true, Enabled: true, Tier: backendstore.TierSubscription}))
		model := id + "-model"
		require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: id, ModelID: model, Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
		adapters = append(adapters, recoveryAdapter{id: id, result: backendusage.Result{Status: backendusage.StatusOK, Usage: limits[id]}})
	}
	st := newFakeStore()
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{ID: "agent-1", AiCli: "codex", Model: "codex-model", Role: "general", Status: store.StatusRateLimited}))
	life := &recoveryLife{failures: make(map[string]error), st: st}
	c := NewBackendRecoveryCoordinator(st, bs, backendusage.NewService(bs, adapters...), life)
	c.stabilizationWindow = 5 * time.Millisecond
	return c, st, life
}

// TestCooldownRepeatedTransitions verifies that each confirmed hard limit stamps
// durable cooldown evidence and that repeated transitions accumulate evidence
// across multiple backend/model candidates without cycling back.
func TestCooldownRepeatedTransitions(t *testing.T) {
	dir := t.TempDir()
	c, st, _ := recoveryFixtureWithDir(t, dir, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	fallbackAt := time.Now().Add(time.Hour).UTC()

	// First hard limit on the original backend (codex).
	require.True(t, c.OnHardLimit(&agentstore.Agent{ID: "agent-1"}, fallbackAt))

	// Cooldown for codex must be persisted immediately (before advance() runs).
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()), "codex must have a durable cooldown after the first hard limit")

	// Wait for recovery to reach stabilizing on claude.
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing && s.AiCli == "claude"
	}, time.Second, 5*time.Millisecond)

	// Simulate claude hitting a hard limit immediately in the stabilizing phase.
	// The coordinator reads the current session's Backend/Model from the store.
	current := st.snapSession("agent-1")
	require.NotNil(t, current.BackendRecovery)
	require.NotNil(t, current.BackendRecovery.Current)

	// Re-trigger OnHardLimit from the claude session perspective to simulate
	// an immediate_hard_limit while stabilizing.
	claudeFallback := time.Now().Add(2 * time.Hour).UTC()
	c.OnHardLimit(current, claudeFallback)

	// Cooldown for claude must now also be persisted.
	require.Eventually(t, func() bool {
		return c.backends.IsRLCoolingDown("claude", "claude-model", time.Now())
	}, time.Second, 5*time.Millisecond, "claude must have a durable cooldown after immediate_hard_limit")

	// Both codex and claude are cooled down; only antigravity is eligible.
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()), "codex cooldown must still be active")
	require.False(t, c.backends.IsRLCoolingDown("antigravity", "antigravity-model", time.Now()), "antigravity must NOT be cooled down")

	// Wait for recovery to stabilize on antigravity, then confirm cooldowns
	// survive the short recovery-stable window (evidence must not clear with
	// BackendRecovery).
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing && s.AiCli == "antigravity"
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	current = st.snapSession("agent-1")
	c.OnTransition(current, store.StatusSpawning, store.StatusWorking)
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery == nil
	}, time.Second, 5*time.Millisecond)
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()), "codex cooldown must survive recovery stabilization")
	require.True(t, c.backends.IsRLCoolingDown("claude", "claude-model", time.Now()), "claude cooldown must survive recovery stabilization")
}

// TestCooldownFallbackResetTime verifies that when no reset time is parseable,
// the provided fallbackAt is used directly as the cooldown expiry.
func TestCooldownFallbackResetTime(t *testing.T) {
	dir := t.TempDir()
	c, _, _ := recoveryFixtureWithDir(t, dir, nil)

	fallback := time.Now().Add(45 * time.Minute).UTC()
	require.True(t, c.OnHardLimit(&agentstore.Agent{ID: "agent-1"}, fallback))

	// The cooldown expiry must be at or after the fallback time (buffer may extend it).
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()), "codex must be in cooldown")

	// One minute before the fallback the cooldown must still be active.
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", fallback.Add(-time.Minute)), "cooldown must still be active before fallback expires")

	// After the fallback the cooldown must have expired.
	require.False(t, c.backends.IsRLCoolingDown("codex", "codex-model", fallback.Add(time.Second)), "cooldown must have expired after fallback time")
}

// TestCooldownRestartPersistence verifies that confirmed-hard-limit cooldown
// evidence written by one coordinator instance survives opening a new store
// (simulating a daemon restart). The new coordinator reads the durable evidence
// from disk and skips the cooled-down candidate.
func TestCooldownRestartPersistence(t *testing.T) {
	dir := t.TempDir()

	fallbackAt := time.Now().Add(time.Hour).UTC()

	// Phase 1: record a cooldown via the first coordinator instance, then close.
	func() {
		bs, err := backendstore.NewStore(dir)
		require.NoError(t, err)

		seeded, err := bs.ListModels("")
		require.NoError(t, err)
		for _, model := range seeded {
			require.NoError(t, bs.SetModelEnabled(model.BackendID, model.ModelID, false))
		}
		require.NoError(t, bs.Upsert(backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierSubscription}))
		require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: "codex", ModelID: "codex-model", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))

		st := newFakeStore()
		require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{ID: "agent-1", AiCli: "codex", Model: "codex-model", Status: store.StatusRateLimited}))
		life := &recoveryLife{failures: make(map[string]error)}
		c1 := NewBackendRecoveryCoordinator(st, bs, backendusage.NewService(bs), life)
		require.True(t, c1.OnHardLimit(&agentstore.Agent{ID: "agent-1"}, fallbackAt))
		require.True(t, bs.IsRLCoolingDown("codex", "codex-model", time.Now()), "cooldown must be persisted before close")
		// Close bs to flush all in-memory state to disk before reopening.
		require.NoError(t, bs.Close())
	}()

	// Phase 2: open a NEW Store from the same directory (simulate daemon restart).
	bs2, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	defer bs2.Close()

	// The cooldown must still be active in the freshly opened store.
	require.True(t, bs2.IsRLCoolingDown("codex", "codex-model", time.Now()), "cooldown must survive daemon restart (new Store from same directory)")

	// After the reset time it must be gone.
	require.False(t, bs2.IsRLCoolingDown("codex", "codex-model", fallbackAt.Add(time.Second)), "cooldown must have expired after reset time in the new store")
}

// TestCooldownAllCandidatesLimited verifies that when every eligible candidate
// is cooled down (confirmed hard limit evidence persisted in the backendstore),
// advance() calls waitLocked and the session enters recoveryWaiting.
func TestCooldownAllCandidatesLimited(t *testing.T) {
	dir := t.TempDir()
	reset := time.Now().Add(time.Hour).UTC()

	// All three backends report 100% usage so the usage snapshot also blocks them.
	// The durable cooldowns make selection loop-safe regardless of round.
	c, st, _ := recoveryFixtureWithDir(t, dir, map[string][]backendusage.Limit{
		"codex":       {{ID: "short", Scope: "short", Label: "Short", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})

	// Pre-stamp durable cooldowns for all three candidates, so isCoolingDown
	// blocks them even after a round increment clears the in-session attempted map.
	require.NoError(t, c.backends.SetRLCooldown("codex", "codex-model", reset))
	require.NoError(t, c.backends.SetRLCooldown("claude", "claude-model", reset))
	require.NoError(t, c.backends.SetRLCooldown("antigravity", "antigravity-model", reset))

	require.True(t, c.OnHardLimit(&agentstore.Agent{ID: "agent-1"}, reset))

	// All candidates are cooled down; the coordinator must reach recoveryWaiting.
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting && s.BackendRecovery.NextRetryAt != nil
	}, time.Second, 5*time.Millisecond)

	// The next retry must be scheduled at or near the reset time.
	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery.NextRetryAt)
	require.True(t, !s.BackendRecovery.NextRetryAt.Before(time.Now()), "next retry must be in the future")
}
