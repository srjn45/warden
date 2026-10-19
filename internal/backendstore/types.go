package backendstore

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

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

// ValidStored also permits the deliberately non-routable unclassified tier.
func (t ModelTier) ValidStored() bool { return t.Valid() || t == TierUnclassified }

// ModelEntry represents a model supported by an agent backend with its tier and status.
type ModelEntry struct {
	BackendID   string    `json:"backend_id"`
	ModelID     string    `json:"model_id"`
	Tier        ModelTier `json:"tier"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	AutoAssign  bool      `json:"auto_assign"`
	IsCustom    bool      `json:"is_custom"`
	// QuotaScope tags which BackendQuota.Scope this model consumes. Blank means
	// "default" at read time (docs/specs/2026-09-26-per-scope-quota-routing.md D2/D7).
	QuotaScope   string        `json:"quota_scope,omitempty"`
	Origin       string        `json:"origin,omitempty"`
	OperatorSet  []string      `json:"operator_set,omitempty"`
	Lifecycle    string        `json:"lifecycle,omitempty"`
	RetiredAt    time.Time     `json:"retired_at,omitzero"`
	RetiredByRev uint64        `json:"retired_by_rev,omitempty"`
	RetiredPrior *RetiredPrior `json:"retired_prior,omitempty"`
	CatalogRev   uint64        `json:"catalog_rev,omitempty"`
}

type RetiredPrior struct {
	AutoAssign bool `json:"auto_assign"`
}

const (
	ModelOriginSeed        = "seed"
	ModelOriginCustom      = "custom"
	ModelOriginDiscovered  = "discovered"
	ModelLifecycleActive   = "active"
	ModelLifecycleRetired  = "retired"
	ModelLifecycleExcluded = "excluded"
)

func (m ModelEntry) EffectiveOrigin() string {
	if m.Origin != "" {
		return m.Origin
	}
	if m.IsCustom {
		return ModelOriginCustom
	}
	return ModelOriginSeed
}
func (m ModelEntry) EffectiveLifecycle() string {
	if m.Lifecycle != "" {
		return m.Lifecycle
	}
	return ModelLifecycleActive
}

// CatalogScope contains non-secret identity inputs. AccountRef and Profile are
// deliberately excluded from persisted JSON; only ScopeFP is stored.
type CatalogScope struct {
	AiCli      string `json:"ai_cli"`
	AuthMode   string `json:"auth_mode,omitempty"`
	AccountRef string `json:"-"`
	Profile    string `json:"-"`
	PlanLabel  string `json:"plan_label,omitempty"`
	CLIVersion string `json:"cli_version,omitempty"`
}

func (s CatalogScope) ScopeFP() string {
	h := sha256.Sum256([]byte("warden-model-scope-v1:" + s.AiCli + ":" + s.AuthMode + ":" + s.AccountRef + ":" + s.Profile))
	return "sha256:" + hex.EncodeToString(h[:12])
}

type SourceKind string

const (
	SourceLiveCommand     SourceKind = "live_command"
	SourceCLICache        SourceKind = "cli_cache"
	SourceProviderCatalog SourceKind = "provider_catalog"
	SourceBundled         SourceKind = "bundled"
	SourceConfigHint      SourceKind = "config_hint"
	SourceLaunchRejection SourceKind = "launch_rejection"
)

type Completeness string

const (
	CompletenessComplete Completeness = "complete"
	CompletenessPartial  Completeness = "partial"
	CompletenessEmpty    Completeness = "empty"
	CompletenessFailed   Completeness = "failed"
)

type CatalogError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type ObservedModel struct {
	ModelID          string            `json:"model_id"`
	DisplayName      string            `json:"display_name,omitempty"`
	Visibility       string            `json:"visibility,omitempty"`
	ReasoningLevels  []string          `json:"reasoning_levels,omitempty"`
	DefaultReasoning string            `json:"default_reasoning,omitempty"`
	ContextWindow    int               `json:"context_window,omitempty"`
	UpgradeTo        string            `json:"upgrade_to,omitempty"`
	Raw              map[string]string `json:"raw,omitempty"`
}
type CatalogObservation struct {
	Source          SourceKind      `json:"source"`
	SourceDetail    string          `json:"source_detail,omitempty"`
	SourceFetchedAt time.Time       `json:"source_fetched_at,omitzero"`
	Completeness    Completeness    `json:"completeness"`
	Err             *CatalogError   `json:"error,omitempty"`
	Models          []ObservedModel `json:"models,omitempty"`
}
type Availability string

const (
	AvailabilityAvailable   Availability = "available"
	AvailabilityUnavailable Availability = "unavailable"
	AvailabilityUnknown     Availability = "unknown"
	AvailabilityStale       Availability = "stale"
	AvailabilityUnsupported Availability = "unsupported"
)

type ModelCapabilities struct {
	ReasoningLevels  []string `json:"reasoning_levels,omitempty"`
	DefaultReasoning string   `json:"default_reasoning,omitempty"`
	ContextWindow    int      `json:"context_window,omitempty"`
	UpgradeTo        string   `json:"upgrade_to,omitempty"`
}
type StoredObservation struct {
	AiCli                 string            `json:"ai_cli"`
	ScopeFP               string            `json:"scope_fp"`
	ModelID               string            `json:"model_id"`
	Availability          Availability      `json:"availability"`
	Visibility            string            `json:"visibility,omitempty"`
	LastSeenAt            time.Time         `json:"last_seen_at,omitzero"`
	LastCheckedAt         time.Time         `json:"last_checked_at,omitzero"`
	EvidenceSnapshot      string            `json:"evidence_snapshot,omitempty"`
	LaunchRejectedUntil   time.Time         `json:"launch_rejected_until,omitzero"`
	LaunchRejectionReason string            `json:"launch_rejection_reason,omitempty"`
	Capabilities          ModelCapabilities `json:"capabilities,omitempty"`
}
type ActiveCatalogScope struct {
	AiCli         string    `json:"ai_cli"`
	ActiveScopeFP string    `json:"active_scope_fp,omitempty"`
	AuthMode      string    `json:"auth_mode,omitempty"`
	PlanLabel     string    `json:"plan_label,omitempty"`
	CLIVersion    string    `json:"cli_version,omitempty"`
	ResolvedAt    time.Time `json:"resolved_at,omitzero"`
}
type CatalogSnapshot struct {
	ID              string          `json:"id"`
	AiCli           string          `json:"ai_cli"`
	ScopeFP         string          `json:"scope_fp,omitempty"`
	AuthMode        string          `json:"auth_mode,omitempty"`
	PlanLabel       string          `json:"plan_label,omitempty"`
	CLIVersion      string          `json:"cli_version,omitempty"`
	Source          SourceKind      `json:"source"`
	SourceDetail    string          `json:"source_detail,omitempty"`
	SourceFetchedAt time.Time       `json:"source_fetched_at,omitzero"`
	ObservedAt      time.Time       `json:"observed_at"`
	Completeness    Completeness    `json:"completeness"`
	Err             *CatalogError   `json:"error,omitempty"`
	Models          []ObservedModel `json:"models,omitempty"`
	ContentHash     string          `json:"content_hash,omitempty"`
	SchemaVersion   int             `json:"schema_version,omitempty"`
}
type CatalogProposal struct {
	ID            string              `json:"id"`
	State         string              `json:"state"`
	CreatedAt     time.Time           `json:"created_at"`
	ExpiresAt     time.Time           `json:"expires_at"`
	AiClis        []string            `json:"ai_clis,omitempty"`
	Snapshots     []string            `json:"snapshots,omitempty"`
	BaseRev       uint64              `json:"base_rev,omitempty"`
	PolicyVersion string              `json:"policy_version,omitempty"`
	Scope         ActiveCatalogScope  `json:"scope,omitempty"`
	ScopeFP       string              `json:"scope_fp,omitempty"`
	Ops           []CatalogProposalOp `json:"ops,omitempty"`
	Blocked       []CatalogBlockedOp  `json:"blocked,omitempty"`
	Observations  []ObservationChange `json:"observation_changes,omitempty"`
}

// CatalogProposalOp is a fully explained, compare-and-swap protected catalog
// row change. Before/After deliberately retain the complete row so revisions
// can roll it back without reconstructing policy from future evidence.
type CatalogProposalOp struct {
	Op       string      `json:"op"`
	Key      string      `json:"key"`
	PreHash  string      `json:"pre_hash"`
	Before   *ModelEntry `json:"before,omitempty"`
	After    *ModelEntry `json:"after,omitempty"`
	Reasons  []string    `json:"reasons,omitempty"`
	Evidence string      `json:"evidence,omitempty"`
}
type CatalogBlockedOp struct{ Key, Op, Reason string }
type ObservationChange struct {
	Key  string       `json:"key"`
	From Availability `json:"from"`
	To   Availability `json:"to"`
}
type CatalogRevision struct {
	Rev        uint64              `json:"rev"`
	ParentRev  uint64              `json:"parent_rev,omitempty"`
	CreatedAt  time.Time           `json:"created_at"`
	Snapshots  []string            `json:"snapshots,omitempty"`
	Kind       string              `json:"kind,omitempty"`
	ProposalID string              `json:"proposal_id,omitempty"`
	Reverts    uint64              `json:"reverts,omitempty"`
	Ops        []CatalogProposalOp `json:"ops,omitempty"`
}

// RoleTierMapping maps an agent role to its default model tier.
type RoleTierMapping struct {
	RoleName    string    `json:"role_name"`
	DefaultTier ModelTier `json:"default_tier"`
}

// HandoverSettings holds configurations for mid-session context handover.
// The ContextFillThreshold is the only active trigger; ThresholdPercent and
// RollingQuotaThreshold are retained for one compatibility window but have no
// effect — confirmed hard-limit recovery owns provider-quota switching.
type HandoverSettings struct {
	Enabled bool `json:"enabled"`
	// Deprecated: has no effect. Provider quota switching is triggered only by
	// confirmed hard limits via the backend recovery coordinator. Kept for
	// one compatibility window; will be removed in a future release.
	ThresholdPercent int `json:"threshold_percent"`
	// Deprecated: has no effect. Provider quota switching is triggered only by
	// confirmed hard limits via the backend recovery coordinator. Kept for
	// one compatibility window; will be removed in a future release.
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
