package router_test

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/stretchr/testify/require"
)

func setupTestStore(t *testing.T) *backendstore.Store {
	t.Helper()
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)

	// Seed backends: claude (subscription), antigravity (subscription), cursor (subscription), codex (subscription)
	backends := []backendstore.Backend{
		{
			ID:        "claude",
			Installed: true,
			Enabled:   true,
			Tier:      backendstore.TierSubscription,
		},
		{
			ID:        "antigravity",
			Installed: true,
			Enabled:   true,
			Tier:      backendstore.TierSubscription,
		},
		{
			ID:        "cursor",
			Installed: true,
			Enabled:   true,
			Tier:      backendstore.TierSubscription,
		},
		{
			ID:        "codex",
			Installed: true,
			Enabled:   true,
			Tier:      backendstore.TierSubscription,
		},
	}
	for _, b := range backends {
		require.NoError(t, s.Upsert(b))
	}
	return s
}

func TestResolver_RoleToTierResolution(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	r := router.NewResolver(s)
	ctx := context.Background()

	// 1. Role: orchestrator -> Tier 1 (seeded default)
	res, err := r.ResolveRole(ctx, "orchestrator")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, res.Tier)
	require.Contains(t, []string{"claude", "antigravity", "cursor", "codex"}, res.BackendID)

	// 2. Role: general -> Tier 2 (seeded default)
	res, err = r.ResolveRole(ctx, "general")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, res.Tier)

	// 3. Role: worker -> Tier 2 (seeded default)
	res, err = r.ResolveRole(ctx, "worker")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, res.Tier)

	// 4. Unknown role falls through to the Tier 2 default.
	res, err = r.ResolveRole(ctx, "no-such-role")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, res.Tier)
}

// TestResolver_DetermineTargetTierPrecedence verifies the tier precedence chain:
// explicit tier > task tier (task.TierFor) > role tier (GetRoleTier) > Tier2 default.
func TestResolver_DetermineTargetTierPrecedence(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	r := router.NewResolver(s)

	// Explicit tier wins over both task and role.
	tier, err := r.DetermineTargetTier(router.ResolveOptions{
		Tier: backendstore.Tier3,
		Task: "analysis",     // task tier 1
		Role: "orchestrator", // role tier 1
	})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier3, tier)

	// Task tier beats role tier: analysis (task tier 1) over general (role tier 2).
	tier, err = r.DetermineTargetTier(router.ResolveOptions{
		Task: "analysis",
		Role: "general",
	})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, tier)

	// Task tier 3 drives the task branch (monitor-ci is a tier-3 task).
	tier, err = r.DetermineTargetTier(router.ResolveOptions{Task: "monitor-ci"})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier3, tier)

	// A task tier-2 example (development).
	tier, err = r.DetermineTargetTier(router.ResolveOptions{Task: "development"})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, tier)

	// Role tier applies when no task is given: orchestrator -> Tier 1.
	tier, err = r.DetermineTargetTier(router.ResolveOptions{Role: "orchestrator"})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, tier)

	// An unknown task name falls through to the role tier.
	tier, err = r.DetermineTargetTier(router.ResolveOptions{
		Task: "does-not-exist",
		Role: "orchestrator",
	})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, tier)

	// Neither task nor role -> Tier 2 default.
	tier, err = r.DetermineTargetTier(router.ResolveOptions{})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, tier)

	// An unknown role also falls through to the Tier 2 default.
	tier, err = r.DetermineTargetTier(router.ResolveOptions{Role: "ghost-role"})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, tier)

	// An explicit invalid tier is rejected.
	_, err = r.DetermineTargetTier(router.ResolveOptions{Tier: "tier-9"})
	require.ErrorIs(t, err, backendstore.ErrInvalidTier)
}

func TestResolver_RoundRobinTieBreaking(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	// Restrict to only claude and antigravity
	require.NoError(t, s.SetEnabled("cursor", false))
	require.NoError(t, s.SetEnabled("codex", false))

	// Ensure antigravity only has 1 enabled model for tier-1 so candidates count is 1 per backend
	require.NoError(t, s.SetModelEnabled("antigravity", "gemini-3.1-pro-high", false))

	r := router.NewResolver(s)
	ctx := context.Background()

	// Both claude and antigravity have 100% headroom (usage = 0)
	// Consecutive resolutions should balance / alternate between them
	res1, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1})
	require.NoError(t, err)

	res2, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1})
	require.NoError(t, err)

	require.NotEqual(t, res1.BackendID, res2.BackendID, "expected round-robin to balance across distinct backends")
	require.ElementsMatch(t, []string{"antigravity", "claude"}, []string{res1.BackendID, res2.BackendID})
}

func TestResolver_HighestHeadroomSelection(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	// Disable cursor and codex for this test
	require.NoError(t, s.SetEnabled("cursor", false))
	require.NoError(t, s.SetEnabled("codex", false))

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	r := router.NewResolver(s).WithNow(func() time.Time { return now })
	ctx := context.Background()

	// Record usage:
	// Claude: 350,000 / 500,000 tokens (70% usage -> 30% headroom)
	require.NoError(t, s.RecordQuotaUsage("claude", 350000, "claude-3-7-sonnet-20250219", now))

	// Antigravity: 100,000 / 1,000,000 tokens (10% usage -> 90% headroom)
	require.NoError(t, s.RecordQuotaUsage("antigravity", 100000, "claude-sonnet-4-6", now))

	// Antigravity has significantly higher headroom (90% vs 30%), so it must be selected
	res, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier2})
	require.NoError(t, err)
	require.Equal(t, "antigravity", res.BackendID)
	require.InDelta(t, 0.9, res.Headroom, 0.001)
}

func TestResolver_Automatic90PercentThresholdFailover(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	// Disable cursor and codex
	require.NoError(t, s.SetEnabled("cursor", false))
	require.NoError(t, s.SetEnabled("codex", false))

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	r := router.NewResolver(s).WithNow(func() time.Time { return now })
	ctx := context.Background()

	// Claude hits 92% quota usage (460,000 / 500,000 tokens)
	require.NoError(t, s.RecordQuotaUsage("claude", 460000, "claude-3-7-sonnet-20250219", now))

	// Antigravity has 50% usage (500,000 / 1,000,000 tokens -> 50% headroom)
	require.NoError(t, s.RecordQuotaUsage("antigravity", 500000, "claude-sonnet-4-6", now))

	// Claude exceeds 90% threshold and must failover to Antigravity
	res, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier2})
	require.NoError(t, err)
	require.Equal(t, "antigravity", res.BackendID)
	require.InDelta(t, 0.5, res.Headroom, 0.001)

	// Verify Claude candidate was marked ineligible due to threshold
	var claudeEval *router.CandidateEvaluation
	for i := range res.Candidates {
		if res.Candidates[i].BackendID == "claude" {
			claudeEval = &res.Candidates[i]
			break
		}
	}
	require.NotNil(t, claudeEval)
	require.False(t, claudeEval.Eligible)
	require.Contains(t, claudeEval.RejectReason, "quota usage 92.0% >= threshold 90%")
}

func TestResolver_CooldownLimitFailover(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	require.NoError(t, s.SetEnabled("cursor", false))
	require.NoError(t, s.SetEnabled("codex", false))

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	r := router.NewResolver(s).WithNow(func() time.Time { return now })
	ctx := context.Background()

	// Set Claude into cooldown for 15 minutes
	require.NoError(t, s.SetBackendLimited("claude", now.Add(15*time.Minute)))

	// Resolver should route to Antigravity
	res, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1})
	require.NoError(t, err)
	require.Equal(t, "antigravity", res.BackendID)

	// Advance time past cooldown -> Claude becomes available again
	rAfter := router.NewResolver(s).WithNow(func() time.Time { return now.Add(20 * time.Minute) })
	resAfter, err := rAfter.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1, PreferredBackend: "claude"})
	require.NoError(t, err)
	require.Equal(t, "claude", resAfter.BackendID)
}

func TestResolver_ExplicitPreferredBackendAndModel(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	r := router.NewResolver(s)
	ctx := context.Background()

	// Explicit preference: antigravity + gemini-3.7-flash-high
	res, err := r.Resolve(ctx, router.ResolveOptions{
		PreferredBackend: "antigravity",
		PreferredModel:   "gemini-3.7-flash-high",
	})
	require.NoError(t, err)
	require.Equal(t, "antigravity", res.BackendID)
	require.Equal(t, "gemini-3.7-flash-high", res.ModelID)
	require.Equal(t, backendstore.Tier2, res.Tier)
}

func TestResolver_AllExhaustedReturnsError(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	r := router.NewResolver(s).WithNow(func() time.Time { return now })
	ctx := context.Background()

	// Push all backends over 90% threshold
	require.NoError(t, s.RecordQuotaUsage("claude", 480000, "claude-opus", now))                   // 96%
	require.NoError(t, s.RecordQuotaUsage("antigravity", 950000, "claude-opus-4-6-thinking", now)) // 95%
	require.NoError(t, s.RecordQuotaUsage("cursor", 490, "claude-3-opus", now))                    // 98%
	require.NoError(t, s.RecordQuotaUsage("codex", 490000, "o1", now))                             // 98%

	_, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1})
	require.ErrorIs(t, err, router.ErrAllExhausted)
}

func TestResolver_TierFallback(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	r := router.NewResolver(s).WithNow(func() time.Time { return now })
	ctx := context.Background()

	// Push Claude and Antigravity over 90% in Tier 1, disable cursor and codex
	require.NoError(t, s.SetEnabled("cursor", false))
	require.NoError(t, s.SetEnabled("codex", false))
	require.NoError(t, s.SetModelEnabled("claude", "claude-opus", false))
	require.NoError(t, s.SetModelEnabled("antigravity", "claude-opus-4-6-thinking", false))
	require.NoError(t, s.SetModelEnabled("antigravity", "gemini-3.1-pro-high", false))

	// Without fallback -> ErrAllExhausted
	_, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1, AllowFallback: false})
	require.Error(t, err)

	// With fallback -> falls back to Tier 2 models
	res, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1, AllowFallback: true})
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, res.Tier)
	require.Contains(t, []string{"claude", "antigravity"}, res.BackendID)
}

func TestResolver_UninstalledOrDisabledBackendFiltered(t *testing.T) {
	s := setupTestStore(t)
	defer s.Close()

	// Mark claude as not installed, and antigravity as disabled
	bClaude, err := s.Get("claude")
	require.NoError(t, err)
	bClaude.Installed = false
	require.NoError(t, s.Upsert(bClaude))

	require.NoError(t, s.SetEnabled("antigravity", false))

	r := router.NewResolver(s)
	ctx := context.Background()

	res, err := r.Resolve(ctx, router.ResolveOptions{Tier: backendstore.Tier1})
	require.NoError(t, err)
	// Must select from remaining installed & enabled backends (cursor or codex)
	require.Contains(t, []string{"cursor", "codex"}, res.BackendID)
}
