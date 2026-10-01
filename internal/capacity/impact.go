package capacity

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Observation sources that feed the same impact seam. Phase 5 calculates
// impact; coordinated-bulk-recovery (Phase 6) consumes AffectedAgents to
// invoke the existing backend recovery coordinator. Pane signal fusion lands
// in a later phase.
const (
	SourceUsage  = "usage"
	SourcePane   = "pane"
	SourceMenu   = "menu"
	SourceBanner = "banner"
	SourceManual = "manual"
)

// Skip reasons mirror the user-visible states from the usage-api quota recovery
// contract. already_reconciled is the idempotency fence outcome.
const (
	SkipDone              = "done"
	SkipArchived          = "archived"
	SkipTerminal          = "terminal"
	SkipStopped           = "stopped"
	SkipUnboundLegacy     = "unbound_legacy"
	SkipRecovering        = "recovering"
	SkipSuperseded        = "superseded"
	SkipAlreadyReconciled = "already_reconciled"
	SkipStatusIneligible  = "status_ineligible"
)

// Bucket observation freshness values used by impact reconciliation. They match
// backendusage.Freshness spellings so callers can pass snapshot fields through.
const (
	ImpactFresh   = "fresh"
	ImpactStale   = "stale"
	ImpactUnknown = "unknown"
)

// Bucket states accepted as exhaustion evidence. Only fresh+exhausted+authoritative
// observations drive affected-agent selection.
const (
	ImpactBucketExhausted = "exhausted"
	ImpactBucketAvailable = "available"
	ImpactBucketUnknown   = "unknown"
)

// AgentView is the narrow live-agent projection impact reconciliation needs.
// It intentionally avoids importing agentstore/store so capacity stays free of
// those cycles; callers map from Agent/Session.
type AgentView struct {
	ID                 string
	Status             string
	Terminal           bool
	Archived           bool
	Binding            *QuotaBinding
	Recovering         bool
	RecoveryGeneration uint64
	Superseded         bool
}

// BucketObservation is one capacity-bucket signal presented for impact.
// Domain identity uses the same opaque account fingerprint as QuotaBinding.
type BucketObservation struct {
	Revision           uint64
	Provider           string
	AccountFingerprint string
	Route              string // empty = shared / account-wide bucket
	BucketKey          string
	State              string
	Freshness          string
	Authoritative      bool
	Source             string
	// ResetsAt is the provider-reported reset time for this bucket, when known.
	// Phase 6 (coordinated-bulk-recovery) threads it through to AffectedAgent so
	// the backend recovery coordinator's confirmed hard-limit entry point gets a
	// real fallback scheduling instant instead of only a generic default when no
	// pane excerpt is available.
	ResetsAt *time.Time
}

// ExhaustedBucket is one fresh, authoritative exhaustion that drove impact.
type ExhaustedBucket struct {
	Provider           string     `json:"provider"`
	AccountFingerprint string     `json:"account_fingerprint"`
	Route              string     `json:"route,omitempty"`
	BucketKey          string     `json:"bucket_key"`
	SnapshotRevision   uint64     `json:"snapshot_revision"`
	Source             string     `json:"source,omitempty"`
	ResetsAt           *time.Time `json:"resets_at,omitempty"`
}

// AffectedAgent is a live bound agent that requires an exhausted bucket.
type AffectedAgent struct {
	AgentID            string `json:"agent_id"`
	Provider           string `json:"provider"`
	AccountFingerprint string `json:"account_fingerprint"`
	Route              string `json:"route,omitempty"`
	BucketKey          string `json:"bucket_key"`
	SnapshotRevision   uint64 `json:"snapshot_revision"`
	RecoveryGeneration uint64 `json:"recovery_generation"`
	Source             string `json:"source,omitempty"`
	// ResetsAt is the exhausted bucket's provider-reported reset time, when
	// known. The coordinated-bulk-recovery wiring (internal/daemon) uses this as
	// the fallback scheduling instant passed to the backend recovery
	// coordinator's OnHardLimit when no later usage snapshot yields a sooner one.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// SkippedAgent records why a live or known agent was not selected.
type SkippedAgent struct {
	AgentID string `json:"agent_id"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// StaleOrUnknownInput captures observations that must not force recovery.
type StaleOrUnknownInput struct {
	Provider           string `json:"provider"`
	AccountFingerprint string `json:"account_fingerprint,omitempty"`
	Route              string `json:"route,omitempty"`
	BucketKey          string `json:"bucket_key,omitempty"`
	SnapshotRevision   uint64 `json:"snapshot_revision,omitempty"`
	Freshness          string `json:"freshness,omitempty"`
	State              string `json:"state,omitempty"`
	Reason             string `json:"reason"`
	Source             string `json:"source,omitempty"`
}

// ImpactResult is the structured output of one reconciliation pass.
type ImpactResult struct {
	ExhaustedBuckets []ExhaustedBucket     `json:"exhausted_buckets"`
	AffectedAgents   []AffectedAgent       `json:"affected_agents"`
	SkippedAgents    []SkippedAgent        `json:"skipped_agents"`
	StaleOrUnknown   []StaleOrUnknownInput `json:"stale_or_unknown"`
}

// FenceStore persists snapshot-revision / domain / bucket / generation fences
// so repeated observations and daemon restarts stay idempotent.
type FenceStore interface {
	Claim(rec FenceRecord) (claimed bool, err error)
}

// DomainKey builds the stable non-secret domain identity used in fences.
func DomainKey(provider, accountFingerprint, route string) string {
	return provider + "\x00" + accountFingerprint + "\x00" + route
}

// RequiresBucket reports whether the binding lists key as mandatory.
func (b *QuotaBinding) RequiresBucket(key string) bool {
	if b == nil {
		return false
	}
	for _, k := range b.MandatoryBuckets {
		if k == key {
			return true
		}
	}
	for _, k := range b.Domain.BucketKeys {
		if k == key {
			return true
		}
	}
	return false
}

// MatchesObservation reports whether the binding's domain requires the observed
// provider/account/(optional route) and bucket. An empty observation route means
// a shared account-level bucket and matches any route on that account.
func (b *QuotaBinding) MatchesObservation(obs BucketObservation) bool {
	if b == nil {
		return false
	}
	provider := b.Domain.Provider
	if provider == "" {
		provider = b.Domain.AiCli
	}
	if provider != obs.Provider {
		return false
	}
	if b.Domain.AccountFingerprint != obs.AccountFingerprint {
		return false
	}
	if obs.Route != "" && b.Domain.Route != obs.Route {
		return false
	}
	return b.RequiresBucket(obs.BucketKey)
}

// ReconcileImpact maps fresh exhausted bucket observations to eligible live
// agents. It never starts recovery itself: internal/daemon's bulk-recovery
// wiring consumes AffectedAgents and invokes the existing backend recovery
// coordinator's confirmed hard-limit entry point per agent.
func ReconcileImpact(agents []AgentView, observations []BucketObservation, fences FenceStore) (ImpactResult, error) {
	out := ImpactResult{
		ExhaustedBuckets: []ExhaustedBucket{},
		AffectedAgents:   []AffectedAgent{},
		SkippedAgents:    []SkippedAgent{},
		StaleOrUnknown:   []StaleOrUnknownInput{},
	}
	if len(observations) == 0 {
		return out, nil
	}

	sortedAgents := append([]AgentView(nil), agents...)
	sort.Slice(sortedAgents, func(i, j int) bool { return sortedAgents[i].ID < sortedAgents[j].ID })

	seenSkip := map[string]struct{}{}
	skip := func(id, reason, detail string) {
		key := id + "\x00" + reason + "\x00" + detail
		if _, ok := seenSkip[key]; ok {
			return
		}
		seenSkip[key] = struct{}{}
		out.SkippedAgents = append(out.SkippedAgents, SkippedAgent{AgentID: id, Reason: reason, Detail: detail})
	}

	seenExhausted := map[string]struct{}{}
	for _, obs := range observations {
		obs = normalizeObservation(obs)
		if reason := rejectionReason(obs); reason != "" {
			out.StaleOrUnknown = append(out.StaleOrUnknown, StaleOrUnknownInput{
				Provider:           obs.Provider,
				AccountFingerprint: obs.AccountFingerprint,
				Route:              obs.Route,
				BucketKey:          obs.BucketKey,
				SnapshotRevision:   obs.Revision,
				Freshness:          obs.Freshness,
				State:              obs.State,
				Reason:             reason,
				Source:             obs.Source,
			})
			continue
		}
		exKey := fmt.Sprintf("%d\x00%s\x00%s", obs.Revision, DomainKey(obs.Provider, obs.AccountFingerprint, obs.Route), obs.BucketKey)
		if _, ok := seenExhausted[exKey]; !ok {
			seenExhausted[exKey] = struct{}{}
			out.ExhaustedBuckets = append(out.ExhaustedBuckets, ExhaustedBucket{
				Provider:           obs.Provider,
				AccountFingerprint: obs.AccountFingerprint,
				Route:              obs.Route,
				BucketKey:          obs.BucketKey,
				SnapshotRevision:   obs.Revision,
				Source:             obs.Source,
				ResetsAt:           obs.ResetsAt,
			})
		}
		detail := obs.BucketKey
		for _, agent := range sortedAgents {
			if reason, d := ineligibleReason(agent); reason != "" {
				skip(agent.ID, reason, d)
				continue
			}
			if agent.Binding == nil {
				skip(agent.ID, SkipUnboundLegacy, LegacyUnbound)
				continue
			}
			if !agent.Binding.MatchesObservation(obs) {
				continue
			}
			if agent.Recovering {
				skip(agent.ID, SkipRecovering, detail)
				continue
			}
			if agent.Superseded {
				skip(agent.ID, SkipSuperseded, detail)
				continue
			}
			rec := FenceRecord{
				SnapshotRevision:   obs.Revision,
				DomainKey:          DomainKey(obs.Provider, obs.AccountFingerprint, obs.Route),
				BucketKey:          obs.BucketKey,
				AgentID:            agent.ID,
				RecoveryGeneration: agent.RecoveryGeneration,
				Source:             obs.Source,
			}
			if fences != nil {
				claimed, err := fences.Claim(rec)
				if err != nil {
					return out, err
				}
				if !claimed {
					skip(agent.ID, SkipAlreadyReconciled, fmt.Sprintf("revision=%d bucket=%s generation=%d", obs.Revision, obs.BucketKey, agent.RecoveryGeneration))
					continue
				}
			}
			out.AffectedAgents = append(out.AffectedAgents, AffectedAgent{
				AgentID:            agent.ID,
				Provider:           obs.Provider,
				AccountFingerprint: obs.AccountFingerprint,
				Route:              firstNonEmpty(obs.Route, agent.Binding.Domain.Route),
				BucketKey:          obs.BucketKey,
				SnapshotRevision:   obs.Revision,
				RecoveryGeneration: agent.RecoveryGeneration,
				Source:             obs.Source,
				ResetsAt:           obs.ResetsAt,
			})
		}
	}
	sort.Slice(out.AffectedAgents, func(i, j int) bool {
		if out.AffectedAgents[i].AgentID != out.AffectedAgents[j].AgentID {
			return out.AffectedAgents[i].AgentID < out.AffectedAgents[j].AgentID
		}
		return out.AffectedAgents[i].BucketKey < out.AffectedAgents[j].BucketKey
	})
	sort.Slice(out.SkippedAgents, func(i, j int) bool {
		if out.SkippedAgents[i].AgentID != out.SkippedAgents[j].AgentID {
			return out.SkippedAgents[i].AgentID < out.SkippedAgents[j].AgentID
		}
		return out.SkippedAgents[i].Reason < out.SkippedAgents[j].Reason
	})
	return out, nil
}

func normalizeObservation(obs BucketObservation) BucketObservation {
	obs.Provider = strings.TrimSpace(obs.Provider)
	obs.AccountFingerprint = strings.TrimSpace(obs.AccountFingerprint)
	obs.Route = strings.TrimSpace(obs.Route)
	obs.BucketKey = strings.TrimSpace(obs.BucketKey)
	obs.State = strings.ToLower(strings.TrimSpace(obs.State))
	obs.Freshness = strings.ToLower(strings.TrimSpace(obs.Freshness))
	obs.Source = strings.TrimSpace(obs.Source)
	if obs.Source == "" {
		obs.Source = SourceUsage
	}
	return obs
}

func rejectionReason(obs BucketObservation) string {
	if obs.Provider == "" || obs.BucketKey == "" {
		return "incomplete_observation"
	}
	if !obs.Authoritative {
		return "not_authoritative"
	}
	switch obs.Freshness {
	case ImpactStale:
		return "stale"
	case ImpactUnknown, "":
		return "unknown"
	case ImpactFresh:
		// ok
	default:
		return "unknown"
	}
	switch obs.State {
	case ImpactBucketExhausted:
		return ""
	case ImpactBucketUnknown, "":
		return "unknown_bucket"
	default:
		return "not_exhausted"
	}
}

func ineligibleReason(a AgentView) (reason, detail string) {
	if a.Terminal {
		return SkipTerminal, "kind=terminal"
	}
	if a.Archived {
		return SkipArchived, a.Status
	}
	switch strings.ToLower(strings.TrimSpace(a.Status)) {
	case "done":
		return SkipDone, a.Status
	case "stopped":
		return SkipStopped, a.Status
	case "errored", "orphaned":
		return SkipStatusIneligible, a.Status
	case "spawning", "working", "idle", "waiting_for_input", "rate_limited", "busy", "need-input":
		return "", ""
	default:
		if a.Status == "" {
			return SkipStatusIneligible, "empty_status"
		}
		return SkipStatusIneligible, a.Status
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
