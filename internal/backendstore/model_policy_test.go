package backendstore

import "testing"

import "github.com/stretchr/testify/require"

func TestRecommendModelIsEvidenceBasedAndConservative(t *testing.T) {
	unknown := RecommendModel(CatalogScope{AiCli: "codex", AuthMode: "chatgpt"}, ObservedModel{ModelID: "gpt-future"}, nil)
	require.Equal(t, ModelPolicyVersion, unknown.PolicyVersion)
	require.Equal(t, ModelTier(TierUnclassified), unknown.Tier)
	require.False(t, unknown.AutoAssign)
	require.Contains(t, unknown.RuleIDs, "R5 no-match")
	require.Equal(t, "codex", unknown.QuotaScope)

	hidden := RecommendModel(CatalogScope{AiCli: "codex", AuthMode: "chatgpt"}, ObservedModel{ModelID: "gpt-5.5", Visibility: "hidden"}, nil)
	require.Equal(t, Tier1, hidden.Tier)
	require.False(t, hidden.AutoAssign)
	require.Contains(t, hidden.RuleIDs, "R1 seed-exact")
	require.Contains(t, hidden.RuleIDs, "R4 hidden")

	api := RecommendModel(CatalogScope{AiCli: "codex", AuthMode: "apikey"}, ObservedModel{ModelID: "gpt-future"}, nil)
	require.Equal(t, "api", api.QuotaScope)
	require.False(t, api.AutoAssign)
	require.Contains(t, api.Blockers, "billing_mode_pay_per_use")
}

func TestRecommendModelUsesProviderSuccessorOnlyWhenSupplied(t *testing.T) {
	prior := &ModelEntry{Tier: Tier1, AutoAssign: true, QuotaScope: "codex"}
	r := RecommendModel(CatalogScope{AiCli: "codex", AuthMode: "chatgpt"}, ObservedModel{ModelID: "gpt-next"}, prior)
	require.Equal(t, Tier1, r.Tier)
	require.True(t, r.AutoAssign)
	require.Contains(t, r.RuleIDs, "R3 provider-upgrade")
}
