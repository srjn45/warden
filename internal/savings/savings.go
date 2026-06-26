// Package savings records the token reductions warden's lifecycle features earn
// in real use — the counterfactual (what would have entered an agent's context)
// versus what actually did. Each feature that shrinks transcript spend appends an
// Event; the aggregate (Summary) is warden's measured, auditable proof of how
// many Claude tokens — and dollars — it saved, per feature and overall.
//
// The package is split pure/impure like internal/metrics: the event model,
// token estimate, and aggregation here are unit-testable with no I/O; store.go
// owns the append-only JSONL ledger.
package savings

import (
	"sort"
	"time"
)

// Feature labels — the set of lifecycle surfaces that record a saving. Kept as
// constants so the emit sites, the aggregator, and any UI legend never drift.
const (
	FeatureCheck  = "check"  // wd check: raw test/lint output → failures-only summary (incl. local-model condensation, already reflected in the kept side)
	FeatureCommit = "commit" // wd commit/push/sync: git tool-spam → compact struct
	// FeatureLLMOffload covers a classify/summarize call served by the local model
	// instead of warden's own Claude — the whole prompt left Claude's spend (kept 0).
	FeatureLLMOffload = "llm_offload"
	// FeatureCompact is reserved for auto-/compact reclaiming context-window fill;
	// not yet emitted (its saving is measured post-compaction in the poller).
	FeatureCompact = "compact"
)

// Savings split into two axes that make fundamentally different claims, so the
// report keeps them separate rather than blending them into one percentage:
//
//   - axisContext: the feature shrank what actually entered an agent's context
//     (KeptTokens > 0). The win is a leaner transcript, measured as a reduction
//     percentage of would-be context spend.
//   - axisOffload: the whole call was served off Claude (KeptTokens == 0), so its
//     entire spend left Claude — both the input AND the output Claude would have
//     generated. There is no "with warden" context to compare against, so it
//     cannot honestly join the context reduction percentage.
const (
	axisContext = "context"
	axisOffload = "offload"
)

// featureAxis maps a Feature* label to its savings axis. Unknown / future
// features fall back to the context axis — the conservative bucket that joins
// the reduction percentage rather than the offload dollar claim.
func featureAxis(feature string) string {
	switch feature {
	case FeatureLLMOffload:
		return axisOffload
	default: // check, commit, compact, and any future context-shrinking feature
		return axisContext
	}
}

// Event is one recorded saving. RawTokens is the counterfactual (what the
// feature avoided putting in context); KeptTokens is what actually entered it
// (0 for an offload, where the whole call left Claude entirely). Saved is
// RawTokens-KeptTokens, never negative.
//
// One Event is one line of the on-disk ledger and the wire shape for GET /savings.
type Event struct {
	TS         time.Time `json:"ts"`
	Feature    string    `json:"feature"`
	Agent      string    `json:"agent,omitempty"`
	RawTokens  int       `json:"raw_tokens"`
	KeptTokens int       `json:"kept_tokens"`
	Saved      int       `json:"saved"`
}

// NewEvent builds an Event, deriving Saved and clamping it at 0 so a feature that
// happened to grow the output (KeptTokens > RawTokens — never expected, but cheap
// to guard) can never record a negative "saving" that would poison the totals.
func NewEvent(feature, agent string, rawTokens, keptTokens int) Event {
	saved := rawTokens - keptTokens
	if saved < 0 {
		saved = 0
	}
	return Event{
		TS:         time.Now().UTC(),
		Feature:    feature,
		Agent:      agent,
		RawTokens:  rawTokens,
		KeptTokens: keptTokens,
		Saved:      saved,
	}
}

// EstimateTokens approximates the token count of a byte slice. We use the
// widely-cited ~4-bytes-per-token heuristic (ceil), which is good enough for a
// savings gauge measured across thousands of events; the estimate is isolated
// here so a future exact count-tokens call can replace it without touching any
// emit site. An empty slice is 0 tokens.
func EstimateTokens(b []byte) int { return EstimateTokensLen(len(b)) }

// EstimateTokensLen is EstimateTokens over a known byte length, for call sites
// that have the size but not the bytes (e.g. a string already truncated).
func EstimateTokensLen(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

// FeatureSummary is the rolled-up saving for one feature over a window.
type FeatureSummary struct {
	Feature      string  `json:"feature"`
	Events       int     `json:"events"`
	RawTokens    int     `json:"raw_tokens"`
	KeptTokens   int     `json:"kept_tokens"`
	SavedTokens  int     `json:"saved_tokens"`
	SavedDollars float64 `json:"saved_dollars"`
}

// Summary is the whole ledger (over a window) aggregated for display: per-feature
// breakdown plus the grand totals, split into the two savings axes (see
// featureAxis) that the report claims separately.
//
// ReductionPct is the legacy blended saved ÷ (saved+kept) across every feature.
// It is kept for back-compat (JSON consumers) but is no longer the headline,
// because it dilutes the offload axis (kept==0) into the context ratio. The
// honest headline is ContextReductionPct alongside the OffloadedDollars line.
type Summary struct {
	Since        time.Time        `json:"since,omitempty"`
	Events       int              `json:"events"`
	RawTokens    int              `json:"raw_tokens"`
	KeptTokens   int              `json:"kept_tokens"`
	SavedTokens  int              `json:"saved_tokens"`
	SavedDollars float64          `json:"saved_dollars"`
	ReductionPct float64          `json:"reduction_pct"`
	Features     []FeatureSummary `json:"features"`

	// Context axis: features that shrank the live transcript (kept>0). These
	// totals are internally consistent — ContextSavedTokens = ContextRawTokens -
	// ContextKeptTokens — so the without/with A/B and the reduction % agree.
	ContextRawTokens    int     `json:"context_raw_tokens"`
	ContextKeptTokens   int     `json:"context_kept_tokens"`
	ContextSavedTokens  int     `json:"context_saved_tokens"`
	ContextSavedDollars float64 `json:"context_saved_dollars"`
	ContextReductionPct float64 `json:"context_reduction_pct"`

	// Offload axis: calls served off Claude entirely (kept==0). OffloadedTokens is
	// the input that left Claude; OffloadedDollars values that input PLUS the
	// output Claude would have generated, the latter from a documented assumption
	// (see assumedOffloadOutputTokens) — it is modeled, not measured.
	OffloadedTokens  int     `json:"offloaded_tokens"`
	OffloadedDollars float64 `json:"offloaded_dollars"`
	OffloadedEvents  int     `json:"offloaded_events"`
}

// dollarsPerToken is the price warden attributes to a saved input token when
// reporting a dollar figure. It is deliberately conservative — set to Claude
// Opus's input rate so the "$ saved" number is a floor a buyer can trust rather
// than a best case. Pricing is in $/token; see PricePerMTok for the per-million
// figure callers configure against.
const dollarsPerToken = pricePerMTok / 1_000_000

// PricePerMTok is the $/million-input-tokens rate Summarize prices saved tokens
// at. Exposed so the CLI can name the assumption in its output.
const PricePerMTok = pricePerMTok

// pricePerMTok is Claude Opus 4.x input pricing ($/MTok) as of 2026-06 (Opus
// 4.8/4.7/4.6 all share the $5/MTok input rate). Saved tokens are overwhelmingly
// input tokens (test logs, git output, prompts that never reach Claude), so the
// input rate is the right multiplier.
const pricePerMTok = 5.0

// OutputPricePerMTok is Claude Opus 4.x OUTPUT pricing ($/MTok) as of 2026-06
// ($25/MTok). It only applies to the offload axis: an offloaded classify/
// summarize call would also have produced output tokens on Claude, so the
// offload dollar figure values that avoided output on top of the avoided input.
// Exposed so the CLI can name the assumption in its output.
const OutputPricePerMTok = 25.0

// outputDollarsPerToken is OutputPricePerMTok in $/token, the offload axis's
// output multiplier.
const outputDollarsPerToken = OutputPricePerMTok / 1_000_000

// assumedOffloadOutputTokens is the ASSUMED — not measured — number of output
// tokens an offloaded call would have generated on Claude. The Event schema
// records no output count (the call never reached Claude), so this is the one
// modeled quantity in the savings math. Classify labels and one-line summaries
// are small, so it is a deliberately low fixed estimate per offload event: it
// floors the offload dollar figure rather than inflating it. Because it is a
// guess, the report wording flags the offload output as assumed, never measured.
const assumedOffloadOutputTokens = 64

// offloadDollars values an offload-axis aggregate: the avoided input tokens at
// the input rate, plus an assumed-small output term (assumedOffloadOutputTokens
// per event) at the output rate. The output term is modeled, not measured.
func offloadDollars(inputTokens, events int) float64 {
	return float64(inputTokens)*dollarsPerToken +
		float64(events*assumedOffloadOutputTokens)*outputDollarsPerToken
}

// Summarize aggregates events (already filtered to the desired window by the
// caller) into per-feature rows and grand totals. since is echoed into the
// result for display only. Feature rows are sorted by SavedTokens descending —
// the biggest win on top — so the report leads with what sells.
func Summarize(events []Event, since time.Time) Summary {
	byFeature := map[string]*FeatureSummary{}
	var sum Summary
	sum.Since = since
	for _, e := range events {
		sum.Events++
		sum.RawTokens += e.RawTokens
		sum.KeptTokens += e.KeptTokens
		sum.SavedTokens += e.Saved
		switch featureAxis(e.Feature) {
		case axisOffload:
			// kept==0, so the whole input left Claude; RawTokens is what offloaded.
			sum.OffloadedEvents++
			sum.OffloadedTokens += e.RawTokens
		default: // axisContext
			sum.ContextRawTokens += e.RawTokens
			sum.ContextKeptTokens += e.KeptTokens
			sum.ContextSavedTokens += e.Saved
		}
		f := byFeature[e.Feature]
		if f == nil {
			f = &FeatureSummary{Feature: e.Feature}
			byFeature[e.Feature] = f
		}
		f.Events++
		f.RawTokens += e.RawTokens
		f.KeptTokens += e.KeptTokens
		f.SavedTokens += e.Saved
	}

	// Context axis: a leaner transcript, priced at the input rate only.
	sum.ContextSavedDollars = float64(sum.ContextSavedTokens) * dollarsPerToken
	if denom := sum.ContextSavedTokens + sum.ContextKeptTokens; denom > 0 {
		sum.ContextReductionPct = float64(sum.ContextSavedTokens) / float64(denom) * 100
	}
	// Offload axis: avoided input + the (assumed-small) output Claude would have
	// produced. See assumedOffloadOutputTokens — the output term is modeled.
	sum.OffloadedDollars = offloadDollars(sum.OffloadedTokens, sum.OffloadedEvents)

	// Blended legacy totals. ReductionPct stays a pure token ratio for back-compat;
	// SavedDollars is the honest sum of both axes (context input + offload in/out),
	// so it equals the per-feature dollar rows below.
	sum.SavedDollars = sum.ContextSavedDollars + sum.OffloadedDollars
	if denom := sum.SavedTokens + sum.KeptTokens; denom > 0 {
		sum.ReductionPct = float64(sum.SavedTokens) / float64(denom) * 100
	}

	sum.Features = make([]FeatureSummary, 0, len(byFeature))
	for _, f := range byFeature {
		// Per-feature pricing is axis-aware: offload rows carry the avoided output.
		if featureAxis(f.Feature) == axisOffload {
			f.SavedDollars = offloadDollars(f.RawTokens, f.Events)
		} else {
			f.SavedDollars = float64(f.SavedTokens) * dollarsPerToken
		}
		sum.Features = append(sum.Features, *f)
	}
	sort.Slice(sum.Features, func(i, j int) bool {
		if sum.Features[i].SavedTokens != sum.Features[j].SavedTokens {
			return sum.Features[i].SavedTokens > sum.Features[j].SavedTokens
		}
		return sum.Features[i].Feature < sum.Features[j].Feature // stable tiebreak
	})
	return sum
}
