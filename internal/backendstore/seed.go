package backendstore

// DefaultModels returns the standard seed catalog of models grouped by backend and tier.
func DefaultModels() []ModelEntry {
	return []ModelEntry{
		// Tier 1 models
		{
			BackendID:   "claude",
			ModelID:     "claude-opus",
			Tier:        Tier1,
			DisplayName: "Claude Opus",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "claude-opus-4-6-thinking",
			Tier:        Tier1,
			DisplayName: "Claude Opus 4.6 (Thinking)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.1-pro-high",
			Tier:        Tier1,
			DisplayName: "Gemini 3.1 Pro (High)",
			Enabled:     true,
		},
		{
			BackendID:   "cursor",
			ModelID:     "claude-3-opus",
			Tier:        Tier1,
			DisplayName: "Claude 3 Opus",
			Enabled:     true,
		},
		{
			BackendID:   "codex",
			ModelID:     "o1",
			Tier:        Tier1,
			DisplayName: "o1",
			Enabled:     true,
		},

		// Tier 2 models
		{
			BackendID:   "claude",
			ModelID:     "claude-3-7-sonnet",
			Tier:        Tier2,
			DisplayName: "Claude 3.7 Sonnet",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "claude-sonnet-4-6",
			Tier:        Tier2,
			DisplayName: "Claude Sonnet 4.6 (Thinking)",
			Enabled:     true,
		},
		{
			BackendID:   "antigravity",
			ModelID:     "gemini-3.7-flash-high",
			Tier:        Tier2,
			DisplayName: "Gemini 3.7 Flash (High)",
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
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-4.1",
			Tier:        Tier2,
			DisplayName: "GPT-4.1",
			Enabled:     true,
		},
		{
			BackendID:   "codex",
			ModelID:     "o3-mini (high)",
			Tier:        Tier2,
			DisplayName: "o3-mini (high)",
			Enabled:     true,
		},

		// Tier 3 models
		{
			BackendID:   "claude",
			ModelID:     "claude-3-5-haiku",
			Tier:        Tier3,
			DisplayName: "Claude 3.5 Haiku",
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
			ModelID:     "gemini-3.7-flash-low",
			Tier:        Tier3,
			DisplayName: "Gemini 3.7 Flash (Low)",
			Enabled:     true,
		},
		{
			BackendID:   "cursor",
			ModelID:     "composer-2.5-fast",
			Tier:        Tier3,
			DisplayName: "Composer 2.5 Fast",
			Enabled:     true,
		},
		{
			BackendID:   "codex",
			ModelID:     "gpt-4.1-mini",
			Tier:        Tier3,
			DisplayName: "GPT-4.1 Mini",
			Enabled:     true,
		},
	}
}

// DefaultRoleTiers returns the default mapping of agent roles to model tiers.
func DefaultRoleTiers() []RoleTierMapping {
	return []RoleTierMapping{
		{RoleName: "analysis", DefaultTier: Tier1},
		{RoleName: "architecture", DefaultTier: Tier1},
		{RoleName: "planning", DefaultTier: Tier1},
		{RoleName: "design", DefaultTier: Tier1},
		{RoleName: "arch-design-review", DefaultTier: Tier1},
		{RoleName: "autopilot", DefaultTier: Tier1},
		{RoleName: "pr-review", DefaultTier: Tier1},
		{RoleName: "implementation", DefaultTier: Tier2},
		{RoleName: "debugger", DefaultTier: Tier2},
		{RoleName: "code-review", DefaultTier: Tier2},
		{RoleName: "ci-triage", DefaultTier: Tier3},
	}
}
