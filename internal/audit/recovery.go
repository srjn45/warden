package audit

import (
	"regexp"
	"strings"
)

// Quota-recovery audit vocabulary (docs/specs/2026-09-29-usage-api-quota-recovery.md).
// These action names are part of the on-disk schema — keep them stable.
const (
	ActionUsageSnapshotReceived = "usage_snapshot_received"
	ActionUsageSnapshotFailed   = "usage_snapshot_failed"
	ActionQuotaBucketExhausted  = "quota_bucket_exhausted"
	ActionQuotaImpactCalculated = "quota_impact_calculated"
	ActionRecoveryStarted       = "recovery_started"
	ActionCandidateAttempted    = "candidate_attempted"
	ActionCandidateResult       = "candidate_result"
	ActionWaitingForCapacity    = "waiting_for_capacity"
	ActionRecoveryStabilized    = "recovery_stabilized"
	ActionRecoverySuperseded    = "recovery_superseded"
)

// Keys that must never appear in recovery audit Detail maps. Matching is
// case-insensitive on the key name after normalizing separators.
var forbiddenDetailKeys = map[string]struct{}{
	"token":         {},
	"tokens":        {},
	"apitoken":      {},
	"api_token":     {},
	"access_token":  {},
	"refresh_token": {},
	"authorization": {},
	"password":      {},
	"secret":        {},
	"credential":    {},
	"credentials":   {},
	"cookie":        {},
	"raw":           {},
	"raw_response":  {},
	"pane":          {},
	"excerpt":       {},
	"fresh_excerpt": {},
	"prompt":        {},
	"transcript":    {},
	"email":         {},
	"account":       {},
	"account_id":    {},
	"account_email": {},
}

// secretValueRe matches common credential-shaped substrings that must never be
// persisted even when attached to an otherwise-allowed key.
var secretValueRe = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{8,}|bearer\s+[a-z0-9._\-]+|api[_-]?key\s*[:=]\s*\S+|eyJ[a-zA-Z0-9_-]{20,}\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+|[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,})`)

// SafeDetail returns a copy of detail with forbidden keys dropped and
// credential-shaped values redacted. Nil input yields nil. Callers must use
// this before writing recovery audit records so tokens/raw credentials never
// reach ~/.warden/audit.jsonl.
func SafeDetail(detail map[string]string) map[string]string {
	if detail == nil {
		return nil
	}
	out := make(map[string]string, len(detail))
	for k, v := range detail {
		if isForbiddenDetailKey(k) {
			continue
		}
		out[k] = RedactSecrets(v)
	}
	return out
}

func isForbiddenDetailKey(key string) bool {
	norm := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	_, ok := forbiddenDetailKeys[norm]
	return ok
}

// RedactSecrets replaces credential-shaped substrings with [REDACTED]. Safe to
// call on already-safe diagnostic strings (opaque fingerprints, bucket keys).
func RedactSecrets(s string) string {
	if s == "" {
		return s
	}
	return secretValueRe.ReplaceAllString(s, "[REDACTED]")
}

// FormatDomain builds a safe, displayable capacity-domain identifier from
// non-secret parts. Empty parts are omitted. Never accepts raw credentials.
func FormatDomain(provider, accountFingerprint, route string) string {
	parts := make([]string, 0, 3)
	if provider != "" {
		parts = append(parts, provider)
	}
	if accountFingerprint != "" {
		parts = append(parts, TruncateFingerprint(accountFingerprint))
	}
	if route != "" {
		parts = append(parts, route)
	}
	return strings.Join(parts, "/")
}

// TruncateFingerprint returns a bounded opaque fingerprint prefix suitable for
// display and audit correlation. Full fingerprints stay out of TUI/audit lines.
func TruncateFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	if len(fp) <= 16 {
		return fp
	}
	return fp[:16]
}
