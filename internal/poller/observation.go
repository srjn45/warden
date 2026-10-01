package poller

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// RateLimitObservation is an immutable record of evidence captured at the instant
// a rate-limit transition is detected by the poller. It carries the fresh pane
// excerpt (raw terminal text captured at detection time) so downstream consumers
// never read the pre-capture session snapshot stored in LastPaneExcerpt.
//
// # Safe exposure rules
//
// FreshExcerpt contains raw terminal conversation content. It MUST NOT be surfaced
// in ordinary API responses, audit events, or TUI diagnostics. The protected
// diagnostic capture path (RateLimitScheduler.CaptureDir) is the only legitimate
// consumer of FreshExcerpt. Use Fingerprint — a bounded opaque hash — for safe
// structured metadata in events, logs, and the API.
//
// # Downstream dependents
//
// Provider-specific detectors (Task 2) and the recovery cooldown policy (Task 3)
// MUST depend on this type — not on store.Session.LastPaneExcerpt — to guarantee
// they receive the detection-time capture. A consumer reads the fresh excerpt via
// obs.FreshExcerpt, the detection timestamp via obs.ObservedAt, the session
// identifier via obs.SessionID, and the classifier outcome via obs.ClassifierResult.
type RateLimitObservation struct {
	// SessionID is the warden agent session identifier.
	SessionID string
	// ObservedAt is the UTC timestamp at which the rate-limit transition was detected.
	ObservedAt time.Time
	// FreshExcerpt is the raw trailing pane text captured at detection time (the last
	// 20 lines, matching what is stored in store.Session.LastPaneExcerpt after this
	// tick's UpdatePane call). Not safe for ordinary audit/API/TUI exposure — use
	// Fingerprint for those paths.
	FreshExcerpt string
	// ClassifierResult is the status the poller classifier assigned; always
	// store.StatusRateLimited for observations produced by the poller.
	ClassifierResult store.Status
	// Fingerprint is a bounded opaque hash derived from FreshExcerpt. Safe to
	// surface in audit events, TUI diagnostics, and API responses.
	Fingerprint string
	// Source is the Phase 9 trigger vocabulary: "menu" or "banner" (pane paths).
	// Empty defaults to banner at the recovery coordinator.
	Source string
}

// NewRateLimitObservation constructs an observation from the session id and the
// fresh pane excerpt captured at detection time. ObservedAt is set to time.Now().UTC().
// Source defaults to "banner" (confirmed hard-limit wording).
func NewRateLimitObservation(sessionID, freshExcerpt string) RateLimitObservation {
	return RateLimitObservation{
		SessionID:        sessionID,
		ObservedAt:       time.Now().UTC(),
		FreshExcerpt:     freshExcerpt,
		ClassifierResult: store.StatusRateLimited,
		Fingerprint:      rateLimitFingerprint(freshExcerpt),
		Source:           "banner",
	}
}

// NewMenuRateLimitObservation is the menu-confirmation counterpart of
// NewRateLimitObservation. Source is "menu" so recovery observability can
// distinguish a Claude wait-for-reset menu selection from a later banner.
func NewMenuRateLimitObservation(sessionID, freshExcerpt string) RateLimitObservation {
	obs := NewRateLimitObservation(sessionID, freshExcerpt)
	obs.Source = "menu"
	return obs
}

// rateLimitFingerprint returns a short bounded identifier for an excerpt.
// Safe to surface in audit events and TUI diagnostics (no raw terminal content).
func rateLimitFingerprint(excerpt string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(excerpt)))
	return fmt.Sprintf("rl:%x", h[:4])
}
