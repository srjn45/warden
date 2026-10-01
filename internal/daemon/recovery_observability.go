package daemon

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/store"
)

// RecoveryEvidence is the safe, non-secret context that explains why a recovery
// generation started. It is stamped onto BackendRecovery and mirrored into
// durable agent events + the append-only audit trail.
type RecoveryEvidence struct {
	Source             string // usage | menu | banner | manual
	Provider           string
	AccountFingerprint string
	Route              string
	BucketKey          string
	Freshness          string
	Reason             string
	RunID              string
	SnapshotRevision   uint64
}

// NormalizeSource returns a canonical trigger source from the Phase 9 vocabulary.
// Unknown or empty values default to "banner" for pane-driven paths that predate
// explicit sourcing; callers that know better should set Source explicitly.
func NormalizeSource(src string) string {
	switch strings.ToLower(strings.TrimSpace(src)) {
	case capacity.SourceUsage:
		return capacity.SourceUsage
	case capacity.SourceMenu:
		return capacity.SourceMenu
	case capacity.SourceBanner:
		return capacity.SourceBanner
	case capacity.SourceManual:
		return capacity.SourceManual
	case capacity.SourcePane:
		// Legacy pane umbrella → banner (confirmed hard-limit wording).
		return capacity.SourceBanner
	default:
		return ""
	}
}

func (e RecoveryEvidence) domainKey() string {
	return audit.FormatDomain(e.Provider, e.AccountFingerprint, e.Route)
}

func (e RecoveryEvidence) applyTo(rec *store.BackendRecovery) {
	if rec == nil {
		return
	}
	if src := NormalizeSource(e.Source); src != "" {
		rec.TriggerSource = src
	}
	if d := e.domainKey(); d != "" {
		rec.CapacityDomain = d
	}
	if e.BucketKey != "" {
		rec.BucketKey = e.BucketKey
	}
	if e.Freshness != "" {
		rec.Freshness = e.Freshness
	}
	if e.Reason != "" {
		rec.Reason = audit.RedactSecrets(e.Reason)
	}
}

func (e RecoveryEvidence) detail(extra map[string]string) map[string]string {
	d := map[string]string{}
	if src := NormalizeSource(e.Source); src != "" {
		d["trigger_source"] = src
	}
	if dom := e.domainKey(); dom != "" {
		d["capacity_domain"] = dom
	}
	if e.Provider != "" {
		d["provider"] = e.Provider
	}
	if e.AccountFingerprint != "" {
		d["account_fingerprint"] = audit.TruncateFingerprint(e.AccountFingerprint)
	}
	if e.Route != "" {
		d["route"] = e.Route
	}
	if e.BucketKey != "" {
		d["bucket_key"] = e.BucketKey
	}
	if e.Freshness != "" {
		d["freshness"] = e.Freshness
	}
	if e.Reason != "" {
		d["reason"] = e.Reason
	}
	if e.RunID != "" {
		d["run_id"] = e.RunID
	}
	if e.SnapshotRevision > 0 {
		d["snapshot_revision"] = strconv.FormatUint(e.SnapshotRevision, 10)
	}
	for k, v := range extra {
		if v != "" {
			d[k] = v
		}
	}
	return audit.SafeDetail(d)
}

func (e RecoveryEvidence) eventDetail(extra map[string]string) string {
	d := e.detail(extra)
	if len(d) == 0 {
		return ""
	}
	// Stable key order for readable, testable event lines.
	keys := []string{
		"generation", "trigger_source", "capacity_domain", "provider",
		"account_fingerprint", "route", "bucket_key", "freshness",
		"snapshot_revision", "candidate", "outcome", "next_retry",
		"action", "reason", "run_id", "agent_id",
		"exhausted_buckets", "affected_agents", "skipped_agents", "stale_or_unknown",
		"source_status",
	}
	parts := make([]string, 0, len(d))
	seen := map[string]bool{}
	for _, k := range keys {
		if v, ok := d[k]; ok {
			parts = append(parts, k+"="+v)
			seen[k] = true
		}
	}
	var rest []string
	for k := range d {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, " ")
}

// recordRecoveryAudit appends one best-effort audit event. Nil writer is a no-op.
func (s *Server) recordRecoveryAudit(action, target string, detail map[string]string) {
	if s == nil || s.audit == nil {
		return
	}
	s.audit.Log(audit.Event{
		Action: action,
		Target: target,
		Detail: audit.SafeDetail(detail),
	})
}

// recordRecoveryObservability writes both a durable agent event (new vocabulary)
// and an append-only audit record. Legacy backend_recovery_* events remain the
// coordinator's responsibility so existing tests and supersede detection keep
// working; this helper adds the Phase 9 surface beside them.
func (c *BackendRecoveryCoordinator) recordRecoveryObservability(id, action string, ev RecoveryEvidence, extra map[string]string) {
	if c == nil {
		return
	}
	detailMap := ev.detail(extra)
	if id != "" {
		detailMap["agent_id"] = id
		detailMap = audit.SafeDetail(detailMap)
	}
	line := ev.eventDetail(extra)
	if line == "" && len(extra) > 0 {
		// Fall back so generation=/candidate= still land when evidence is empty.
		parts := make([]string, 0, len(extra))
		for k, v := range audit.SafeDetail(extra) {
			parts = append(parts, k+"="+v)
		}
		line = strings.Join(parts, " ")
	}
	if line != "" {
		c.event(id, action, line)
	} else {
		c.event(id, action, "")
	}
	if c.audit != nil {
		c.audit.Log(audit.Event{Action: action, Target: id, Detail: detailMap})
	}
}

func evidenceFromBinding(source, freshness, reason string, provider, fp, route, bucket string, rev uint64) RecoveryEvidence {
	return RecoveryEvidence{
		Source:             source,
		Provider:           provider,
		AccountFingerprint: fp,
		Route:              route,
		BucketKey:          bucket,
		Freshness:          freshness,
		Reason:             reason,
		SnapshotRevision:   rev,
	}
}

func evidenceFromRecovery(rec *store.BackendRecovery) RecoveryEvidence {
	if rec == nil {
		return RecoveryEvidence{}
	}
	ev := RecoveryEvidence{
		Source:    rec.TriggerSource,
		BucketKey: rec.BucketKey,
		Freshness: rec.Freshness,
		Reason:    rec.Reason,
	}
	// CapacityDomain is already a safe display key; split only for audit fields.
	if rec.CapacityDomain != "" {
		parts := strings.Split(rec.CapacityDomain, "/")
		if len(parts) > 0 {
			ev.Provider = parts[0]
		}
		if len(parts) > 1 {
			ev.AccountFingerprint = parts[1]
		}
		if len(parts) > 2 {
			ev.Route = parts[2]
		}
	}
	return ev
}

func summarizeImpact(result capacity.ImpactResult) map[string]string {
	return map[string]string{
		"exhausted_buckets": strconv.Itoa(len(result.ExhaustedBuckets)),
		"affected_agents":   strconv.Itoa(len(result.AffectedAgents)),
		"skipped_agents":    strconv.Itoa(len(result.SkippedAgents)),
		"stale_or_unknown":  strconv.Itoa(len(result.StaleOrUnknown)),
	}
}

func formatSkipReasons(skipped []capacity.SkippedAgent) string {
	if len(skipped) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, s := range skipped {
		counts[s.Reason]++
	}
	parts := make([]string, 0, len(counts))
	for reason, n := range counts {
		parts = append(parts, fmt.Sprintf("%s:%d", reason, n))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
