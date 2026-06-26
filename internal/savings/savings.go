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
	FeatureCheck      = "check"       // wd check: raw test/lint output → failures-only summary
	FeatureCommit     = "commit"      // wd commit/push/sync: git tool-spam → compact struct
	FeatureCondense   = "condense"    // local-model condensation of an oversized check log
	FeatureLLMOffload = "llm_offload" // classify/summarize/commit-msg routed to a free local model
	FeatureCompact    = "compact"     // auto-/compact reclaiming context-window fill
)

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
// breakdown plus the grand totals. ReductionPct is saved ÷ (saved+kept) — the
// share of would-be context spend that warden eliminated — and is the headline
// figure (0 when nothing was kept or recorded).
type Summary struct {
	Since        time.Time        `json:"since,omitempty"`
	Events       int              `json:"events"`
	RawTokens    int              `json:"raw_tokens"`
	KeptTokens   int              `json:"kept_tokens"`
	SavedTokens  int              `json:"saved_tokens"`
	SavedDollars float64          `json:"saved_dollars"`
	ReductionPct float64          `json:"reduction_pct"`
	Features     []FeatureSummary `json:"features"`
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
	sum.SavedDollars = float64(sum.SavedTokens) * dollarsPerToken
	if denom := sum.SavedTokens + sum.KeptTokens; denom > 0 {
		sum.ReductionPct = float64(sum.SavedTokens) / float64(denom) * 100
	}
	sum.Features = make([]FeatureSummary, 0, len(byFeature))
	for _, f := range byFeature {
		f.SavedDollars = float64(f.SavedTokens) * dollarsPerToken
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
