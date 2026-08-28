package backendstore

// DefaultModels returns the standard seed catalog of models grouped by backend and tier.
func DefaultModels() []ModelEntry {
	return []ModelEntry{
		// Tier 1 models
		{
			BackendID:   "claude",
			ModelID:     "opus",
			Tier:        Tier1,
			DisplayName: "Claude Opus",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "claude-opus-4-6-thinking",
			Tier:        Tier1,
			DisplayName: "Claude Opus 4.6 (Thinking)",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.1-pro-high",
			Tier:        Tier1,
			DisplayName: "Gemini 3.1 Pro (High)",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "cursor",
			ModelID:     "claude-3-opus",
			Tier:        Tier1,
			DisplayName: "Claude 3 Opus",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-5.5",
			Tier:        Tier1,
			DisplayName: "GPT-5.5",
			Enabled:     true,
			AutoAssign:  true,
		},

		// Tier 2 models
		{
			BackendID:   "claude",
			ModelID:     "sonnet",
			Tier:        Tier2,
			DisplayName: "Claude 3.7 Sonnet",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "claude-sonnet-4-6",
			Tier:        Tier2,
			DisplayName: "Claude Sonnet 4.6 (Thinking)",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.7-flash-high",
			Tier:        Tier2,
			DisplayName: "Gemini 3.7 Flash (High)",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.6-flash-high",
			Tier:        Tier2,
			DisplayName: "Gemini 3.6 Flash (High)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.1-pro-low",
			Tier:        Tier2,
			DisplayName: "Gemini 3.1 Pro (Low)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gpt-oss-120b-medium",
			Tier:        Tier2,
			DisplayName: "GPT-OSS 120B (Medium)",
			Enabled:     true,
		},
		{
			BackendID:   "cursor",
			ModelID:     "sonnet-3.7",
			Tier:        Tier2,
			DisplayName: "Sonnet 3.7",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-5.6-terra",
			Tier:        Tier2,
			DisplayName: "GPT-5.6 Terra",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-5.4",
			Tier:        Tier2,
			DisplayName: "GPT-5.4",
			Enabled:     true,
		},

		// Tier 3 models
		{
			BackendID:   "claude",
			ModelID:     "haiku",
			Tier:        Tier3,
			DisplayName: "Claude 3.5 Haiku",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.7-flash-medium",
			Tier:        Tier3,
			DisplayName: "Gemini 3.7 Flash (Medium)",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.7-flash-low",
			Tier:        Tier3,
			DisplayName: "Gemini 3.7 Flash (Low)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.6-flash-medium",
			Tier:        Tier3,
			DisplayName: "Gemini 3.6 Flash (Medium)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.6-flash-low",
			Tier:        Tier3,
			DisplayName: "Gemini 3.6 Flash (Low)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.5-flash-high",
			Tier:        Tier3,
			DisplayName: "Gemini 3.5 Flash (High)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.5-flash-medium",
			Tier:        Tier3,
			DisplayName: "Gemini 3.5 Flash (Medium)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.5-flash-low",
			Tier:        Tier3,
			DisplayName: "Gemini 3.5 Flash (Low)",
			Enabled:     true,
		},
		{
			BackendID:   "cursor",
			ModelID:     "composer-2.5-fast",
			Tier:        Tier3,
			DisplayName: "Composer 2.5 Fast",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-5.6-luna",
			Tier:        Tier3,
			DisplayName: "GPT-5.6 Luna",
			Enabled:     true,
			AutoAssign:  true,
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-5.4-mini",
			Tier:        Tier3,
			DisplayName: "GPT-5.4 Mini",
			Enabled:     true,
		},
	}
}

// DefaultRoleTiers returns the default mapping of agent ROLES to model tiers,
// keyed by the actual role names defined under internal/role/roles/*.yaml.
//
// These feed GetRoleTier(role), which the router consults as the Role tier in
// its precedence chain (explicit tier > task tier > role tier > Tier2 default).
// Task-to-tier mappings do NOT live here — internal/task (task.TierFor) is the
// single canonical source for that. A role without an explicit entry here falls
// through to Tier2 in the resolver.
//
// Migration note: earlier builds mis-seeded this collection with TASK-like keys
// (analysis, architecture, planning, design, arch-design-review, autopilot,
// pr-review, implementation, debugger, code-review, ci-triage). Seeding is
// idempotent and never deletes existing keys, so those stale keys survive in
// already-seeded stores but are simply dead: nothing queries role_tiers by task
// name once tasks route through task.TierFor. They cause no runtime breakage.
func DefaultRoleTiers() []RoleTierMapping {
	return []RoleTierMapping{
		{RoleName: "general", DefaultTier: Tier2},
		{RoleName: "orchestrator", DefaultTier: Tier1},
		{RoleName: "planner", DefaultTier: Tier1},
		{RoleName: "worker", DefaultTier: Tier2},
		{RoleName: "autopilot", DefaultTier: Tier1},
		{RoleName: "brain", DefaultTier: Tier2},
	}
}
