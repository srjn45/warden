package backendstore

import "strings"

// ModelPolicyVersion identifies the deterministic, user-auditable catalog
// policy.  Keep this stable: proposals persist rule IDs, not guesses.
const ModelPolicyVersion = "model-policy-v1"

// PolicyRecommendation is the non-mutating result of applying the catalog
// policy to one observed model. Empty fields deliberately mean "no evidence".
// Callers must preserve fields listed in OperatorSet.
type PolicyRecommendation struct {
	PolicyVersion string    `json:"policy_version"`
	Tier          ModelTier `json:"tier"`
	AutoAssign    bool      `json:"auto_assign"`
	QuotaScope    string    `json:"quota_scope,omitempty"`
	RuleIDs       []string  `json:"rule_ids"`
	Blockers      []string  `json:"blockers,omitempty"`
}

// RecommendModel applies R1-R5. It never derives capability, cost, or
// reasoning support from a name/version/menu order. successor is supplied by
// reconciliation only when provider evidence retires the predecessor in the
// same proposal (R3).
func RecommendModel(scope CatalogScope, observed ObservedModel, predecessor *ModelEntry) PolicyRecommendation {
	r := PolicyRecommendation{PolicyVersion: ModelPolicyVersion}
	if seed, ok := defaultModel(observed.ModelID, scope.AiCli); ok {
		r.Tier, r.AutoAssign, r.QuotaScope = seed.Tier, seed.AutoAssign, seed.QuotaScope
		r.RuleIDs = append(r.RuleIDs, "R1 seed-exact")
	} else if predecessor != nil {
		r.Tier, r.AutoAssign = predecessor.Tier, predecessor.AutoAssign
		r.QuotaScope = predecessor.QuotaScope
		r.RuleIDs = append(r.RuleIDs, "R3 provider-upgrade")
	} else {
		r.Tier, r.AutoAssign = TierUnclassified, false
		r.RuleIDs = append(r.RuleIDs, "R5 no-match")
	}

	if quotaScope, applied := policyQuotaScope(scope.AiCli, scope.AuthMode, observed.ModelID); applied {
		r.QuotaScope = quotaScope
		r.RuleIDs = append(r.RuleIDs, "R2 quota-family")
		if scope.AiCli == "codex" && scope.AuthMode == "apikey" {
			r.AutoAssign = false
			r.Blockers = append(r.Blockers, "billing_mode_pay_per_use")
		}
	}
	if strings.EqualFold(observed.Visibility, "hidden") {
		r.AutoAssign = false
		r.RuleIDs = append(r.RuleIDs, "R4 hidden")
	}
	return r
}

func defaultModel(modelID, aiCli string) (ModelEntry, bool) {
	for _, m := range DefaultModels() {
		if m.BackendID == aiCli && m.ModelID == modelID {
			return m, true
		}
	}
	return ModelEntry{}, false
}

func policyQuotaScope(aiCli, authMode, modelID string) (string, bool) {
	switch aiCli {
	case "codex":
		if authMode == "chatgpt" {
			return "codex", true
		}
		if authMode == "apikey" {
			return "api", true
		}
	case "cursor":
		return cursorQuotaScope(modelID), true
	case "antigravity":
		if strings.HasPrefix(modelID, "gemini-") {
			return "gemini", true
		}
		return "non-gemini", true
	case "claude":
		return "session", true
	}
	return "", false
}
