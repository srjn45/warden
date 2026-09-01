package lifecycle

import (
	"fmt"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

// This file holds the context-fill threshold policy behind hot-swap: the pure
// decision that turns a live context-fill measurement,
// evaluated against HandoverSettings, into a signal that the poller (or an operator
// surface) acts on by calling Lifecycle.HotSwap. It is deliberately separate from the
// mechanics in switch.go so the policy is unit-testable in isolation and reusable by
// both the daemon's poller and any CLI/MCP surface added in Stage 4.

// HotSwapSignal is the outcome of evaluating an agent against the handover
// thresholds. Trigger is the actionable bit; the rest is diagnostics for the event
// log / operator surface.
type HotSwapSignal struct {
	Trigger        bool       // fire a hot-swap now
	Reason         SwapReason // which threshold crossed (context_fill | quota); "" when not triggered
	Detail         string     // human-readable explanation for the event log
	ContextFillPct int        // measured context-window fill (0–100+), when known
	QuotaUsedPct   int        // deprecated; reactive hard-limit recovery owns quota
}

// ThresholdInput carries the measurements and policy for one hot-swap evaluation.
// Zero/absent measurements are marked not-known so the policy never triggers on a
// value it does not actually have.
type ThresholdInput struct {
	Settings backendstore.HandoverSettings // enabled flag + fill/quota thresholds + cooldown

	// Context-window fill.
	ContextTokens int  // latest context-window occupancy (tokens)
	ContextLimit  int  // the model's context-window size (tokens); 0 ⇒ fill unknown
	ContextKnown  bool // whether a context-fill reading is available this tick

	// Provider quota headroom.
	QuotaUsed  float64 // provider quota consumed (same unit as QuotaLimit)
	QuotaLimit float64 // provider quota ceiling; 0 ⇒ quota unknown
	QuotaKnown bool    // whether a quota reading is available this tick

	// Cooldown guard: how long since this agent last hot-swapped. A zero SinceSwap
	// with HasSwapped=false means "never swapped" (cooldown satisfied).
	SinceSwap  time.Duration
	HasSwapped bool
}

// DecideHotSwap is the pure context-fill policy. Provider quota measurements are
// intentionally ignored: confirmed StatusRateLimited transitions are handled by
// the backend recovery coordinator.
//
// Thresholds default to 90 when a setting is 0/unset. A measurement that is not known
// this tick (ContextKnown/QuotaKnown false, or a zero limit) is simply not evaluated,
// so the policy never triggers on a fill or quota it cannot actually see.
func DecideHotSwap(in ThresholdInput) HotSwapSignal {
	var sig HotSwapSignal

	if !in.Settings.Enabled {
		return sig
	}

	fillPct, fillKnown := contextFillPercent(in)
	if fillKnown {
		sig.ContextFillPct = fillPct
	}

	// Cooldown: a recent swap suppresses another (avoids swap thrashing when the
	// successor also fills up quickly, or a flapping quota reading).
	if in.HasSwapped && in.SinceSwap < cooldownOr(in.Settings) {
		return sig
	}

	fillThreshold := thresholdOr(in.Settings.ContextFillThreshold)
	if fillKnown && fillPct >= fillThreshold {
		sig.Trigger = true
		sig.Reason = SwapReasonContextFill
		sig.Detail = fillDetail(fillPct, fillThreshold)
		return sig
	}
	return sig
}

// contextFillPercent returns the integer context-window fill percentage and whether
// it is known (a reading exists and the limit is positive).
func contextFillPercent(in ThresholdInput) (int, bool) {
	if !in.ContextKnown || in.ContextLimit <= 0 {
		return 0, false
	}
	return int(float64(in.ContextTokens) / float64(in.ContextLimit) * 100.0), true
}

// quotaUsedPercent returns the integer provider-quota usage percentage and whether it
// is known (a reading exists and the limit is positive).
func quotaUsedPercent(in ThresholdInput) (int, bool) {
	if !in.QuotaKnown || in.QuotaLimit <= 0 {
		return 0, false
	}
	return int(in.QuotaUsed / in.QuotaLimit * 100.0), true
}

// thresholdOr returns pct when it is a sane percentage (1–100), else the 90% default.
func thresholdOr(pct int) int {
	if pct <= 0 || pct > 100 {
		return 90
	}
	return pct
}

// cooldownOr returns the configured cooldown, or the 15-minute default when unset.
func cooldownOr(s backendstore.HandoverSettings) time.Duration {
	if s.CooldownPeriod <= 0 {
		return 15 * time.Minute
	}
	return s.CooldownPeriod
}

func fillDetail(pct, threshold int) string {
	return fmt.Sprintf("context window %d%% full (>= %d%% threshold) — hot-swapping to a fresh session", pct, threshold)
}

func quotaDetail(pct, threshold int) string {
	return fmt.Sprintf("provider quota %d%% used (>= %d%% threshold) — hot-swapping to a backend with headroom", pct, threshold)
}
