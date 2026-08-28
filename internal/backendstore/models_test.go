package backendstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestModelTier_Valid(t *testing.T) {
	require.True(t, Tier1.Valid())
	require.True(t, Tier2.Valid())
	require.True(t, Tier3.Valid())
	require.False(t, ModelTier("tier-4").Valid())
	require.False(t, ModelTier("").Valid())
	require.False(t, ModelTier("t1").Valid())
}

func TestSeedDefaultsOnFreshStore(t *testing.T) {
	s := newTestStore(t)

	// Verify default models
	models, err := s.ListModels("")
	require.NoError(t, err)
	require.Len(t, models, 17)

	// Verify Tier 1 models (5 models)
	tier1Models, err := s.ListModels(Tier1)
	require.NoError(t, err)
	require.Len(t, tier1Models, 5)
	for _, m := range tier1Models {
		require.Equal(t, Tier1, m.Tier)
		require.True(t, m.Enabled)
	}

	// Verify Tier 2 models (7 models)
	tier2Models, err := s.ListModels(Tier2)
	require.NoError(t, err)
	require.Len(t, tier2Models, 7)
	for _, m := range tier2Models {
		require.Equal(t, Tier2, m.Tier)
		require.True(t, m.Enabled)
	}

	// Verify Tier 3 models (5 models)
	tier3Models, err := s.ListModels(Tier3)
	require.NoError(t, err)
	require.Len(t, tier3Models, 5)
	for _, m := range tier3Models {
		require.Equal(t, Tier3, m.Tier)
		require.True(t, m.Enabled)
	}

	// Invalid tier filter
	_, err = s.ListModels("invalid-tier")
	require.ErrorIs(t, err, ErrInvalidTier)

	// Verify specific seeded models
	m, err := s.GetModel("claude", "opus")
	require.NoError(t, err)
	require.Equal(t, "claude", m.BackendID)
	require.Equal(t, "opus", m.ModelID)
	require.Equal(t, Tier1, m.Tier)
	require.Equal(t, "Claude Opus", m.DisplayName)
	require.True(t, m.Enabled)

	m, err = s.GetModel("antigravity", "claude-opus-4-6-thinking")
	require.NoError(t, err)
	require.Equal(t, Tier1, m.Tier)
	require.Equal(t, "Claude Opus 4.6 (Thinking)", m.DisplayName)

	m, err = s.GetModel("antigravity", "gemini-3.1-pro-high")
	require.NoError(t, err)
	require.Equal(t, Tier1, m.Tier)
	require.Equal(t, "Gemini 3.1 Pro (High)", m.DisplayName)

	m, err = s.GetModel("cursor", "claude-3-opus")
	require.NoError(t, err)
	require.Equal(t, Tier1, m.Tier)

	m, err = s.GetModel("codex", "o1")
	require.NoError(t, err)
	require.Equal(t, Tier1, m.Tier)

	m, err = s.GetModel("claude", "sonnet")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)

	m, err = s.GetModel("antigravity", "claude-sonnet-4-6")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)
	require.Equal(t, "Claude Sonnet 4.6 (Thinking)", m.DisplayName)

	m, err = s.GetModel("antigravity", "gemini-3.7-flash-high")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)
	require.Equal(t, "Gemini 3.7 Flash (High)", m.DisplayName)

	m, err = s.GetModel("antigravity", "gpt-oss-120b-medium")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)
	require.Equal(t, "GPT-OSS 120B (Medium)", m.DisplayName)

	m, err = s.GetModel("cursor", "sonnet-3.7")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)

	m, err = s.GetModel("codex", "gpt-4.1")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)

	m, err = s.GetModel("codex", "o3-mini (high)")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)

	m, err = s.GetModel("claude", "haiku")
	require.NoError(t, err)
	require.Equal(t, Tier3, m.Tier)

	m, err = s.GetModel("antigravity", "gemini-3.5-flash-high")
	require.NoError(t, err)
	require.Equal(t, Tier3, m.Tier)
	require.Equal(t, "Gemini 3.5 Flash (High)", m.DisplayName)

	m, err = s.GetModel("antigravity", "gemini-3.7-flash-low")
	require.NoError(t, err)
	require.Equal(t, Tier3, m.Tier)
	require.Equal(t, "Gemini 3.7 Flash (Low)", m.DisplayName)

	m, err = s.GetModel("cursor", "composer-2.5-fast")
	require.NoError(t, err)
	require.Equal(t, Tier3, m.Tier)

	m, err = s.GetModel("codex", "gpt-4.1-mini")
	require.NoError(t, err)
	require.Equal(t, Tier3, m.Tier)

	// Verify default role tiers (6 roles), keyed by real role names.
	roleTiers, err := s.ListRoleTiers()
	require.NoError(t, err)
	require.Len(t, roleTiers, 6)

	expectedRoles := map[string]ModelTier{
		"general":      Tier2,
		"orchestrator": Tier1,
		"planner":      Tier1,
		"worker":       Tier2,
		"autopilot":    Tier1,
		"brain":        Tier2,
	}

	for role, expectedTier := range expectedRoles {
		tier, err := s.GetRoleTier(role)
		require.NoError(t, err, "role %s", role)
		require.Equal(t, expectedTier, tier, "role %s", role)
	}

	// Verify default handover settings
	handover, err := s.GetHandoverSettings()
	require.NoError(t, err)
	require.True(t, handover.Enabled)
	require.Equal(t, 90, handover.ThresholdPercent)
	require.Equal(t, 90, handover.RollingQuotaThreshold)
	require.Equal(t, 90, handover.ContextFillThreshold)
	require.Equal(t, 15*time.Minute, handover.CooldownPeriod)
}

func TestModelOperations(t *testing.T) {
	s := newTestStore(t)

	// Get non-existent model
	_, err := s.GetModel("claude", "unknown-model")
	require.ErrorIs(t, err, ErrModelNotFound)
	_, err = s.GetModel("", "opus")
	require.ErrorIs(t, err, ErrModelNotFound)

	// SetModelTier
	require.NoError(t, s.SetModelTier("claude", "opus", Tier2))
	m, err := s.GetModel("claude", "opus")
	require.NoError(t, err)
	require.Equal(t, Tier2, m.Tier)

	// SetModelTier with invalid tier
	require.ErrorIs(t, s.SetModelTier("claude", "opus", "invalid-tier"), ErrInvalidTier)

	// SetModelTier for non-existent model
	require.ErrorIs(t, s.SetModelTier("claude", "ghost-model", Tier1), ErrModelNotFound)

	// SetModelEnabled
	require.NoError(t, s.SetModelEnabled("claude", "opus", false))
	m, err = s.GetModel("claude", "opus")
	require.NoError(t, err)
	require.False(t, m.Enabled)

	require.NoError(t, s.SetModelEnabled("claude", "opus", true))
	m, err = s.GetModel("claude", "opus")
	require.NoError(t, err)
	require.True(t, m.Enabled)

	require.ErrorIs(t, s.SetModelEnabled("claude", "ghost-model", false), ErrModelNotFound)

	// UpsertModel (insert new)
	newModel := ModelEntry{
		BackendID:   "custom",
		ModelID:     "custom-ultra",
		Tier:        Tier1,
		DisplayName: "Custom Ultra",
		Enabled:     true,
	}
	require.NoError(t, s.UpsertModel(newModel))

	got, err := s.GetModel("custom", "custom-ultra")
	require.NoError(t, err)
	require.Equal(t, "custom", got.BackendID)
	require.Equal(t, "custom-ultra", got.ModelID)
	require.Equal(t, Tier1, got.Tier)
	require.Equal(t, "Custom Ultra", got.DisplayName)
	require.True(t, got.Enabled)

	// UpsertModel with invalid tier / missing IDs
	require.ErrorIs(t, s.UpsertModel(ModelEntry{BackendID: "custom", ModelID: "test", Tier: "invalid"}), ErrInvalidTier)
	require.Error(t, s.UpsertModel(ModelEntry{BackendID: "", ModelID: "test", Tier: Tier1}))
	require.Error(t, s.UpsertModel(ModelEntry{BackendID: "custom", ModelID: "", Tier: Tier1}))
}

func TestRoleTierOperations(t *testing.T) {
	s := newTestStore(t)

	// Get non-existent role
	_, err := s.GetRoleTier("ghost-role")
	require.ErrorIs(t, err, ErrRoleNotFound)
	_, err = s.GetRoleTier("")
	require.ErrorIs(t, err, ErrRoleNotFound)

	// Update existing (seeded) role tier: general defaults to Tier2.
	require.NoError(t, s.SetRoleTier("general", Tier3))
	tier, err := s.GetRoleTier("general")
	require.NoError(t, err)
	require.Equal(t, Tier3, tier)

	// Set new role tier
	require.NoError(t, s.SetRoleTier("security-audit", Tier1))
	tier, err = s.GetRoleTier("security-audit")
	require.NoError(t, err)
	require.Equal(t, Tier1, tier)

	// List role tiers includes new role (6 seeded defaults + 1 custom).
	roles, err := s.ListRoleTiers()
	require.NoError(t, err)
	require.Len(t, roles, 7)

	// Errors
	require.ErrorIs(t, s.SetRoleTier("analysis", "invalid-tier"), ErrInvalidTier)
	require.Error(t, s.SetRoleTier("", Tier1))
}

func TestHandoverSettingsOperations(t *testing.T) {
	s := newTestStore(t)

	customSettings := HandoverSettings{
		Enabled:               false,
		ThresholdPercent:      85,
		RollingQuotaThreshold: 80,
		ContextFillThreshold:  75,
		CooldownPeriod:        30 * time.Minute,
	}

	require.NoError(t, s.SetHandoverSettings(customSettings))

	got, err := s.GetHandoverSettings()
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Equal(t, 85, got.ThresholdPercent)
	require.Equal(t, 80, got.RollingQuotaThreshold)
	require.Equal(t, 75, got.ContextFillThreshold)
	require.Equal(t, 30*time.Minute, got.CooldownPeriod)
}

func TestReopenPreservesModelAndTierChanges(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	// Modify model, role tier, and handover settings
	require.NoError(t, s.SetModelTier("claude", "opus", Tier3))
	require.NoError(t, s.SetModelEnabled("claude", "opus", false))
	require.NoError(t, s.SetRoleTier("general", Tier3))
	require.NoError(t, s.SetHandoverSettings(HandoverSettings{
		Enabled:               false,
		ThresholdPercent:      95,
		RollingQuotaThreshold: 85,
		ContextFillThreshold:  92,
		CooldownPeriod:        20 * time.Minute,
	}))
	require.NoError(t, s.Close())

	// Reopen store
	s2, err := NewStore(dir)
	require.NoError(t, err)
	defer s2.Close()

	// Verify changes persisted and were NOT overwritten by initial seed
	m, err := s2.GetModel("claude", "opus")
	require.NoError(t, err)
	require.Equal(t, Tier3, m.Tier)
	require.False(t, m.Enabled)

	roleTier, err := s2.GetRoleTier("general")
	require.NoError(t, err)
	require.Equal(t, Tier3, roleTier)

	settings, err := s2.GetHandoverSettings()
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 95, settings.ThresholdPercent)
	require.Equal(t, 85, settings.RollingQuotaThreshold)
	require.Equal(t, 92, settings.ContextFillThreshold)
	require.Equal(t, 20*time.Minute, settings.CooldownPeriod)
}

func TestReopenSyncsMissingSeedModelsCleanly(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)

	// 1. Mutate an existing default model and role tier
	require.NoError(t, s.SetModelTier("claude", "opus", Tier3))
	require.NoError(t, s.SetModelEnabled("claude", "opus", false))
	require.NoError(t, s.SetRoleTier("general", Tier3))

	// 2. Add custom model, custom role, and custom quota
	require.NoError(t, s.UpsertModel(ModelEntry{
		BackendID:   "custom",
		ModelID:     "custom-1",
		Tier:        Tier1,
		DisplayName: "Custom Model 1",
		Enabled:     true,
	}))
	require.NoError(t, s.SetRoleTier("custom-role", Tier1))
	require.NoError(t, s.SetQuota(BackendQuota{
		BackendID:  "custom",
		QuotaLimit: 1000,
		WindowType: WindowDaily,
	}))

	// 3. Remove a few default models, a default role tier, and a default quota from the store
	missingModelKey1 := modelKey("antigravity", "gemini-3.7-flash-high")
	missingModelKey2 := modelKey("claude", "sonnet")
	require.NoError(t, s.modelsCol.DeleteByKey(missingModelKey1))
	require.NoError(t, s.modelsCol.DeleteByKey(missingModelKey2))
	require.NoError(t, s.rolesCol.DeleteByKey("orchestrator"))
	require.NoError(t, s.quotasCol.DeleteByKey("cursor"))

	// Verify they are deleted before reopening
	_, err = s.GetModel("antigravity", "gemini-3.7-flash-high")
	require.ErrorIs(t, err, ErrModelNotFound)
	_, err = s.GetModel("claude", "sonnet")
	require.ErrorIs(t, err, ErrModelNotFound)
	_, err = s.GetRoleTier("orchestrator")
	require.ErrorIs(t, err, ErrRoleNotFound)
	_, err = s.GetQuota("cursor")
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, s.Close())

	// 4. Reopen the store
	s2, err := NewStore(dir)
	require.NoError(t, err)
	defer s2.Close()

	// 5. Verify the missing default models were restored cleanly with defaults
	m1, err := s2.GetModel("antigravity", "gemini-3.7-flash-high")
	require.NoError(t, err)
	require.Equal(t, Tier2, m1.Tier)
	require.Equal(t, "Gemini 3.7 Flash (High)", m1.DisplayName)
	require.True(t, m1.Enabled)

	m2, err := s2.GetModel("claude", "sonnet")
	require.NoError(t, err)
	require.Equal(t, Tier2, m2.Tier)
	require.Equal(t, "Claude 3.7 Sonnet", m2.DisplayName)
	require.True(t, m2.Enabled)

	// 6. Verify missing role tier and quota were restored cleanly
	rt, err := s2.GetRoleTier("orchestrator")
	require.NoError(t, err)
	require.Equal(t, Tier1, rt)

	q, err := s2.GetQuota("cursor")
	require.NoError(t, err)
	require.Equal(t, WindowMonthly, q.WindowType)
	require.Equal(t, 500.0, q.QuotaLimit)

	// 7. Verify mutated default model and role tier were NOT overwritten
	mutatedModel, err := s2.GetModel("claude", "opus")
	require.NoError(t, err)
	require.Equal(t, Tier3, mutatedModel.Tier)
	require.False(t, mutatedModel.Enabled)

	mutatedRole, err := s2.GetRoleTier("general")
	require.NoError(t, err)
	require.Equal(t, Tier3, mutatedRole)

	// 8. Verify custom model, role, and quota were preserved
	customModel, err := s2.GetModel("custom", "custom-1")
	require.NoError(t, err)
	require.Equal(t, Tier1, customModel.Tier)
	require.Equal(t, "Custom Model 1", customModel.DisplayName)

	customRoleTier, err := s2.GetRoleTier("custom-role")
	require.NoError(t, err)
	require.Equal(t, Tier1, customRoleTier)

	customQ, err := s2.GetQuota("custom")
	require.NoError(t, err)
	require.Equal(t, 1000.0, customQ.QuotaLimit)

	// 9. Total counts: 17 defaults + 1 custom = 18 models; 6 defaults + 1 custom = 7 roles; 4 defaults + 1 custom = 5 quotas
	models, err := s2.ListModels("")
	require.NoError(t, err)
	require.Len(t, models, 18)

	roles, err := s2.ListRoleTiers()
	require.NoError(t, err)
	require.Len(t, roles, 7)

	quotas, err := s2.ListQuotas()
	require.NoError(t, err)
	require.Len(t, quotas, 5)
}
