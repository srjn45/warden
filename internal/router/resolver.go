package router

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/task"
)

var (
	// ErrNoCandidate is returned when no candidate model matches the requested criteria.
	ErrNoCandidate = errors.New("router: no eligible candidate available")
	// ErrAllExhausted is returned when all potential candidates exceed the quota threshold (>= 90%) or are limited.
	ErrAllExhausted = errors.New("router: all candidate backends are exhausted or limited")
	// ErrBackendNotFound is returned when an explicitly preferred backend does not exist.
	ErrBackendNotFound = errors.New("router: requested backend not found or not installed")
	// ErrModelNotFound is returned when an explicitly preferred model does not exist in the catalog.
	ErrModelNotFound = errors.New("router: requested model not found in catalog")
)

// Store defines the subset of backendstore operations needed by the resolver.
type Store interface {
	List() ([]backendstore.Backend, error)
	Get(id string) (backendstore.Backend, error)
	ListModels(tierFilter backendstore.ModelTier) ([]backendstore.ModelEntry, error)
	GetModel(backendID, modelID string) (backendstore.ModelEntry, error)
	GetRoleTier(roleName string) (backendstore.ModelTier, error)
	GetHandoverSettings() (backendstore.HandoverSettings, error)
	GetHeadroom(backendID string, now time.Time) (headroom float64, used float64, limit float64, limited bool, err error)
}

// ResolveOptions configures the resolution request.
type ResolveOptions struct {
	Role             string                 // agent role (e.g. "general", "orchestrator", "worker")
	Task             string                 // task name (e.g. "analysis", "development", "code-review"); tier resolved via task.TierFor
	Tier             backendstore.ModelTier // explicit tier override ("tier-1", "tier-2", "tier-3")
	PreferredBackend string                 // explicit backend ID preference ("claude", "antigravity", ...)
	PreferredModel   string                 // explicit model ID preference
	AllowPaid        bool                   // allow pay_per_use backends (default: false, subscription/free only)
	AllowFallback    bool                   // fallback to adjacent tier if target tier is exhausted
	ThresholdPercent int                    // override quota threshold (defaults to HandoverSettings.RollingQuotaThreshold, usually 90)
}

// CandidateEvaluation represents the status and scoring of a candidate model during resolution.
type CandidateEvaluation struct {
	BackendID    string                 `json:"backend_id"`
	ModelID      string                 `json:"model_id"`
	DisplayName  string                 `json:"display_name"`
	Tier         backendstore.ModelTier `json:"tier"`
	Installed    bool                   `json:"installed"`
	Enabled      bool                   `json:"enabled"`
	BackendTier  string                 `json:"backend_tier"` // "subscription", "free", "pay_per_use"
	Used         float64                `json:"used"`
	Limit        float64                `json:"limit"`
	Headroom     float64                `json:"headroom"`    // 1.0 - (used / limit)
	UsageRatio   float64                `json:"usage_ratio"` // used / limit
	Limited      bool                   `json:"limited"`
	LimitedUntil time.Time              `json:"limited_until,omitempty"`
	Eligible     bool                   `json:"eligible"`
	RejectReason string                 `json:"reject_reason,omitempty"`
}

// Resolution is the output of the resolver selection process.
type Resolution struct {
	BackendID   string                 `json:"backend_id"`
	ModelID     string                 `json:"model_id"`
	DisplayName string                 `json:"display_name"`
	Tier        backendstore.ModelTier `json:"tier"`
	Headroom    float64                `json:"headroom"`
	UsageRatio  float64                `json:"usage_ratio"`
	Reason      string                 `json:"reason"`
	Candidates  []CandidateEvaluation  `json:"candidates,omitempty"`
}

// Resolver selects the optimal backend and model for agent creation using quota headroom and tier policies.
type Resolver struct {
	store     Store
	now       func() time.Time
	rrCounter uint64
}

// NewResolver constructs a Resolver over the given backend store.
func NewResolver(store Store) *Resolver {
	return &Resolver{
		store: store,
		now:   time.Now,
	}
}

// WithNow overrides the time source (useful for deterministic tests).
func (r *Resolver) WithNow(now func() time.Time) *Resolver {
	r.now = now
	return r
}

// ResolveRole resolves the optimal backend and model for a given agent role name.
func (r *Resolver) ResolveRole(ctx context.Context, role string) (*Resolution, error) {
	return r.Resolve(ctx, ResolveOptions{Role: role})
}

// ResolveTier resolves the optimal backend and model for an explicit model tier.
func (r *Resolver) ResolveTier(ctx context.Context, tier backendstore.ModelTier) (*Resolution, error) {
	return r.Resolve(ctx, ResolveOptions{Tier: tier})
}

// DetermineTargetTier determines the target model tier, applying this precedence
// (top wins):
//
//	explicit opts.Tier
//	  > Task tier   (task.TierFor, if opts.Task != "")
//	  > Role tier   (store.GetRoleTier, if opts.Role != "")
//	  > Tier2 default
func (r *Resolver) DetermineTargetTier(opts ResolveOptions) (backendstore.ModelTier, error) {
	if opts.Tier != "" {
		if !opts.Tier.Valid() {
			return "", backendstore.ErrInvalidTier
		}
		return opts.Tier, nil
	}
	// Task registry is the canonical task->tier source.
	if opts.Task != "" {
		if n, ok := task.TierFor(opts.Task); ok {
			if tier := tierFromInt(n); tier.Valid() {
				return tier, nil
			}
		}
	}
	if opts.Role != "" {
		tier, err := r.store.GetRoleTier(opts.Role)
		if err == nil && tier.Valid() {
			return tier, nil
		}
	}
	// Default to Tier 2 (standard development & implementation tier)
	return backendstore.Tier2, nil
}

// tierFromInt maps a task registry tier (1, 2, or 3) to a backendstore.ModelTier.
// Any out-of-range value yields the empty tier, which fails Valid().
func tierFromInt(n int) backendstore.ModelTier {
	switch n {
	case 1:
		return backendstore.Tier1
	case 2:
		return backendstore.Tier2
	case 3:
		return backendstore.Tier3
	default:
		return ""
	}
}

// EvaluateCandidates evaluates all model candidates for a specific tier.
func (r *Resolver) EvaluateCandidates(ctx context.Context, targetTier backendstore.ModelTier, opts ResolveOptions) ([]CandidateEvaluation, error) {
	now := r.now().UTC()

	settings, err := r.store.GetHandoverSettings()
	thresholdPercent := 90
	if err == nil && settings.RollingQuotaThreshold > 0 {
		thresholdPercent = settings.RollingQuotaThreshold
	}
	if opts.ThresholdPercent > 0 {
		thresholdPercent = opts.ThresholdPercent
	}
	maxUsageRatio := float64(thresholdPercent) / 100.0

	// Get all backends
	backends, err := r.store.List()
	if err != nil {
		return nil, fmt.Errorf("list backends: %w", err)
	}
	backendMap := make(map[string]backendstore.Backend, len(backends))
	for _, b := range backends {
		backendMap[b.ID] = b
	}

	// Get models for this tier
	models, err := r.store.ListModels(targetTier)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}

	evals := make([]CandidateEvaluation, 0, len(models))

	for _, m := range models {
		eval := CandidateEvaluation{
			BackendID:   m.BackendID,
			ModelID:     m.ModelID,
			DisplayName: m.DisplayName,
			Tier:        m.Tier,
			Enabled:     m.Enabled,
		}

		b, found := backendMap[m.BackendID]
		if !found {
			eval.Eligible = false
			eval.RejectReason = "backend not registered"
			evals = append(evals, eval)
			continue
		}

		eval.Installed = b.Installed
		eval.BackendTier = b.Tier
		eval.LimitedUntil = b.LimitedUntil

		if !m.Enabled {
			eval.Eligible = false
			eval.RejectReason = "model disabled"
			evals = append(evals, eval)
			continue
		}

		if !b.Installed {
			eval.Eligible = false
			eval.RejectReason = "backend binary not installed"
			evals = append(evals, eval)
			continue
		}

		if !b.Enabled {
			eval.Eligible = false
			eval.RejectReason = "backend disabled"
			evals = append(evals, eval)
			continue
		}

		if b.IsLocal {
			eval.Eligible = false
			eval.RejectReason = "local backend excluded from standard agent routing"
			evals = append(evals, eval)
			continue
		}

		// Check tier eligibility (subscription or free; pay_per_use only if explicitly allowed)
		if b.Tier != backendstore.TierSubscription && b.Tier != backendstore.TierFree {
			if !(opts.AllowPaid && b.Tier == backendstore.TierPayPerUse) {
				eval.Eligible = false
				eval.RejectReason = fmt.Sprintf("backend tier '%s' not eligible for subscription/free routing", b.Tier)
				evals = append(evals, eval)
				continue
			}
		}

		// Check preferred filters if provided
		if opts.PreferredBackend != "" && m.BackendID != opts.PreferredBackend {
			eval.Eligible = false
			eval.RejectReason = fmt.Sprintf("does not match preferred backend '%s'", opts.PreferredBackend)
			evals = append(evals, eval)
			continue
		}

		if opts.PreferredModel != "" && m.ModelID != opts.PreferredModel {
			eval.Eligible = false
			eval.RejectReason = fmt.Sprintf("does not match preferred model '%s'", opts.PreferredModel)
			evals = append(evals, eval)
			continue
		}

		// Retrieve quota headroom and cooldown status
		headroom, used, limit, limited, err := r.store.GetHeadroom(m.BackendID, now)
		if err != nil {
			eval.Eligible = false
			eval.RejectReason = fmt.Sprintf("failed to get headroom: %v", err)
			evals = append(evals, eval)
			continue
		}

		eval.Headroom = headroom
		eval.Used = used
		eval.Limit = limit
		eval.Limited = limited
		if limit > 0 {
			eval.UsageRatio = used / limit
		}

		if limited || b.LimitedUntil.After(now) {
			eval.Eligible = false
			eval.RejectReason = fmt.Sprintf("backend is rate-limited / cooldown until %s", b.LimitedUntil.Format(time.RFC3339))
			evals = append(evals, eval)
			continue
		}

		if eval.UsageRatio >= maxUsageRatio {
			eval.Eligible = false
			eval.RejectReason = fmt.Sprintf("quota usage %.1f%% >= threshold %d%% (headroom: %.1f%%)", eval.UsageRatio*100, thresholdPercent, eval.Headroom*100)
			evals = append(evals, eval)
			continue
		}

		// Candidate passed all checks!
		eval.Eligible = true
		evals = append(evals, eval)
	}

	return evals, nil
}

// Resolve evaluates candidates according to opts and selects the winning candidate using weighted headroom and round-robin tie-breaking.
func (r *Resolver) Resolve(ctx context.Context, opts ResolveOptions) (*Resolution, error) {
	targetTier, err := r.DetermineTargetTier(opts)
	if err != nil {
		return nil, err
	}

	evals, err := r.EvaluateCandidates(ctx, targetTier, opts)
	if err != nil {
		return nil, err
	}

	eligible := make([]CandidateEvaluation, 0, len(evals))
	for _, e := range evals {
		if e.Eligible {
			eligible = append(eligible, e)
		}
	}

	// If no eligible candidates in target tier, try fallback tiers if enabled
	if len(eligible) == 0 && opts.AllowFallback && opts.PreferredBackend == "" && opts.PreferredModel == "" {
		fallbackTiers := getFallbackTiers(targetTier)
		for _, fallbackTier := range fallbackTiers {
			fbEvals, fbErr := r.EvaluateCandidates(ctx, fallbackTier, opts)
			if fbErr == nil {
				for _, e := range fbEvals {
					if e.Eligible {
						eligible = append(eligible, e)
					}
				}
				if len(eligible) > 0 {
					targetTier = fallbackTier
					evals = append(evals, fbEvals...)
					break
				}
			}
		}
	}

	if len(eligible) == 0 {
		if len(evals) == 0 {
			return nil, fmt.Errorf("%w: no models found for tier %s", ErrNoCandidate, targetTier)
		}
		return &Resolution{
			Tier:       targetTier,
			Candidates: evals,
		}, ErrAllExhausted
	}

	// Sort eligible candidates by highest headroom first
	// Tied candidates (within delta epsilon 0.0001) will be resolved via round-robin
	const epsilon = 0.0001
	maxHeadroom := -1.0
	for _, e := range eligible {
		if e.Headroom > maxHeadroom {
			maxHeadroom = e.Headroom
		}
	}

	var topTied []CandidateEvaluation
	for _, e := range eligible {
		if math.Abs(e.Headroom-maxHeadroom) <= epsilon {
			topTied = append(topTied, e)
		}
	}

	// Sort tied candidates deterministically by BackendID, ModelID
	sort.Slice(topTied, func(i, j int) bool {
		if topTied[i].BackendID == topTied[j].BackendID {
			return topTied[i].ModelID < topTied[j].ModelID
		}
		return topTied[i].BackendID < topTied[j].BackendID
	})

	// Round-robin tie-breaking across top candidates
	rrIdx := atomic.AddUint64(&r.rrCounter, 1) - 1
	winner := topTied[int(rrIdx%uint64(len(topTied)))]

	reasonDesc := fmt.Sprintf("selected %s:%s (headroom: %.1f%%, tier: %s)", winner.BackendID, winner.ModelID, winner.Headroom*100, winner.Tier)
	if opts.Role != "" {
		reasonDesc += fmt.Sprintf(" for role '%s'", opts.Role)
	}

	return &Resolution{
		BackendID:   winner.BackendID,
		ModelID:     winner.ModelID,
		DisplayName: winner.DisplayName,
		Tier:        winner.Tier,
		Headroom:    winner.Headroom,
		UsageRatio:  winner.UsageRatio,
		Reason:      reasonDesc,
		Candidates:  evals,
	}, nil
}

func getFallbackTiers(t backendstore.ModelTier) []backendstore.ModelTier {
	switch t {
	case backendstore.Tier1:
		return []backendstore.ModelTier{backendstore.Tier2, backendstore.Tier3}
	case backendstore.Tier2:
		return []backendstore.ModelTier{backendstore.Tier3}
	default:
		return nil
	}
}
