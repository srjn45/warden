package backendstore

import "time"

// ModelTier represents the tier classification of a model.
type ModelTier string

const (
	Tier1 ModelTier = "tier-1"
	Tier2 ModelTier = "tier-2"
	Tier3 ModelTier = "tier-3"
)

// Valid reports whether the model tier is one of Tier1, Tier2, or Tier3.
func (t ModelTier) Valid() bool {
	return t == Tier1 || t == Tier2 || t == Tier3
}

// ModelEntry represents a model supported by an agent backend with its tier and status.
type ModelEntry struct {
	BackendID   string    `json:"backend_id"`
	ModelID     string    `json:"model_id"`
	Tier        ModelTier `json:"tier"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	AutoAssign  bool      `json:"auto_assign"`
	IsCustom    bool      `json:"is_custom"`
}

// RoleTierMapping maps an agent role to its default model tier.
type RoleTierMapping struct {
	RoleName    string    `json:"role_name"`
	DefaultTier ModelTier `json:"default_tier"`
}

// HandoverSettings holds configurations for mid-session context handover and quota headroom triggers.
type HandoverSettings struct {
	Enabled bool `json:"enabled"`
	// Deprecated: provider quota switching is triggered only by confirmed hard limits.
	ThresholdPercent int `json:"threshold_percent"`
	// Deprecated: provider quota switching is triggered only by confirmed hard limits.
	RollingQuotaThreshold int           `json:"rolling_quota_threshold"`
	ContextFillThreshold  int           `json:"context_fill_threshold"`
	CooldownPeriod        time.Duration `json:"cooldown_period"`
}

// DefaultHandoverSettings returns the standard default handover configuration.
func DefaultHandoverSettings() HandoverSettings {
	return HandoverSettings{
		Enabled:               true,
		ThresholdPercent:      90,
		RollingQuotaThreshold: 90,
		ContextFillThreshold:  90,
		CooldownPeriod:        15 * time.Minute,
	}
}
