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
	"math"
	"sort"
	"strings"
	"sync"
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
// (0 for an offload, where the whole call left Claude entirely). CostTokens is
// the one-time billed token cost warden's own action incurred to earn the
// saving — non-zero only for compact, where generating the summary bills output
// tokens once (not reflected in the kept-context side). For the local-model
// features (check, llm_offload) the work runs off Claude entirely, so the saving
// is already net and CostTokens is 0. Saved is RawTokens-KeptTokens-CostTokens,
// never negative — the true NET tokens warden kept off Claude's bill.
//
// One Event is one line of the on-disk ledger and the wire shape for GET /savings.
type Event struct {
	TS         time.Time `json:"ts"`
	Feature    string    `json:"feature"`
	Agent      string    `json:"agent,omitempty"`
	RawTokens  int       `json:"raw_tokens"`
	KeptTokens int       `json:"kept_tokens"`
	CostTokens int       `json:"cost_tokens,omitempty"`
	Saved      int       `json:"saved"`
	// RawSample/KeptSample are an opt-in provenance pair: a TRUNCATED snapshot of
	// the real raw output the feature avoided and the real kept output it returned,
	// so a skeptic can eyeball actual bytes behind the token counts (wd savings
	// --audit). Retained only when the `savings_samples` gate is on, only on ~1 in
	// sampleEvery events, and never larger than sampleCap — never the full output.
	// They may hold sensitive substrings of build/test/git output, which is why the
	// capture is off by default. omitempty so an un-sampled event stays compact.
	RawSample  string `json:"raw_sample,omitempty"`
	KeptSample string `json:"kept_sample,omitempty"`
}

const (
	// sampleCap bounds a retained provenance sample (bytes, ~2KB). Enough to see
	// that the raw side really is bulky and the kept side really is lean, without
	// turning the append-only ledger into a copy of every output it measures.
	sampleCap = 2048
	// sampleEvery retains a sample on ~1 in N sample-eligible events, bounding
	// ledger growth when savings_samples is on. Small so even a low-volume install
	// captures a handful — the audit view only needs a few real pairs to convince.
	sampleEvery = 4
)

// TruncateSample returns at most sampleCap bytes from the head of s, dropping a
// trailing partial UTF-8 rune left by the cut so the stored sample is always
// valid UTF-8. Empty in → empty out. The emit sites call this to bound a retained
// raw/kept sample before it reaches the ledger.
func TruncateSample(s string) string {
	if len(s) <= sampleCap {
		return s
	}
	// ToValidUTF8 strips the run of invalid bytes a mid-rune cut leaves at the tail.
	return strings.ToValidUTF8(s[:sampleCap], "")
}

// NewEvent builds an Event, deriving the NET Saved as RawTokens-KeptTokens-
// CostTokens and clamping it at 0. The clamp guards two ways: a feature that
// happened to grow the output (KeptTokens > RawTokens — never expected) and a
// generation cost that exceeded the gross saving (a compaction that reclaimed
// less context than the summary cost to produce) can never record a negative
// "saving" that would poison the totals. costTokens must be a measured figure;
// callers that cannot measure the cost pass 0 (conservative — warden never
// guesses the cost upward, which could only shrink a real saving it earned).
func NewEvent(feature, agent string, rawTokens, keptTokens, costTokens int) Event {
	saved := rawTokens - keptTokens - costTokens
	if saved < 0 {
		saved = 0
	}
	return Event{
		TS:         time.Now().UTC(),
		Feature:    feature,
		Agent:      agent,
		RawTokens:  rawTokens,
		KeptTokens: keptTokens,
		CostTokens: costTokens,
		Saved:      saved,
	}
}

// EstimateTokens approximates the token count of a byte slice. By default it uses
// the widely-cited ~4-bytes-per-token heuristic (ceil); once `wd savings
// --calibrate` has measured this workload's true bytes-per-token ratio against
// Claude's count_tokens endpoint, that empirical factor is used instead (see
// SetCalibration). The estimate is isolated here — the single place bytes become
// tokens — so calibration changes every emit site's accounting without touching
// any of them. An empty slice is 0 tokens.
func EstimateTokens(b []byte) int { return EstimateTokensLen(len(b)) }

// EstimateTokensLen is EstimateTokens over a known byte length, for call sites
// that have the size but not the bytes (e.g. a string already truncated). When
// uncalibrated (no factor set) it is exactly (n+3)/4, byte-for-byte the prior
// behavior; when calibrated it divides bytes by the measured bytes-per-token,
// rounding to the nearest token (never below 1 for a non-empty input).
func EstimateTokensLen(n int) int {
	return estimateTokensLen(n, currentBytesPerToken())
}

// estimateTokensLen is the pure core of the estimate, parameterized on the factor
// so the calibrated and heuristic paths are both unit-testable without touching
// the package-global state. bytesPerToken <= 0 selects the (n+3)/4 heuristic.
func estimateTokensLen(n int, bytesPerToken float64) int {
	if n <= 0 {
		return 0
	}
	if bytesPerToken > 0 {
		t := int(math.Round(float64(n) / bytesPerToken))
		if t < 1 {
			t = 1 // a non-empty input is at least one token
		}
		return t
	}
	return (n + 3) / 4
}

// activeBytesPerToken is the live calibrated bytes-per-token factor consulted by
// EstimateTokensLen. 0 ⇒ uncalibrated, so estimation falls back to the (n+3)/4
// heuristic and behaves exactly as it did before any calibration. Guarded by
// calibMu; the daemon sets it from the persisted calibration sidecar (see
// calibrate.go). This is the single seam where calibration enters the otherwise
// pure token estimate.
var (
	calibMu             sync.RWMutex
	activeBytesPerToken float64
)

// SetCalibration installs an empirical bytes-per-token factor for all subsequent
// estimates. A non-positive value is ignored (calibration only ever replaces the
// heuristic with a real measurement, never with garbage). The daemon calls this
// when it loads the persisted calibration; `wd savings --calibrate` derives and
// persists the factor that ends up here.
func SetCalibration(bytesPerToken float64) {
	if bytesPerToken <= 0 {
		return
	}
	calibMu.Lock()
	defer calibMu.Unlock()
	activeBytesPerToken = bytesPerToken
}

// ClearCalibration reverts estimation to the heuristic. Used by tests to restore
// the package-global default; production code never un-calibrates.
func ClearCalibration() {
	calibMu.Lock()
	defer calibMu.Unlock()
	activeBytesPerToken = 0
}

// currentBytesPerToken reads the live factor under the read lock.
func currentBytesPerToken() float64 {
	calibMu.RLock()
	defer calibMu.RUnlock()
	return activeBytesPerToken
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

	// MeasuredSpend is the cumulative input+output tokens warden actually
	// observed billed to Claude across all agents' transcripts (the spend
	// tracker's grand total). It is a REAL denominator — distinct from the
	// counterfactual RawTokens — used to express the saving as a share of true
	// measured spend (SavedTokens / (SavedTokens+MeasuredSpend)). 0 ⇒ no spend
	// data available (no readable transcript usage yet); the CLI falls back to
	// the context-reduction wording. Populated by the daemon at report time, not
	// by Summarize, since it is sourced outside the event ledger.
	MeasuredSpend int `json:"measured_spend,omitempty"`

	// Calibrated reports whether token estimates use an empirically derived
	// bytes-per-token factor (wd savings --calibrate) instead of the generic
	// 4-bytes/token heuristic, so the report can state its basis unambiguously.
	// CalibratedBytesPerToken and CalibrationSamples describe that basis. Populated
	// by the daemon at report time from the persisted calibration sidecar (like
	// MeasuredSpend), not by Summarize — calibration is sourced outside the event
	// ledger. Calibration is FORWARD-ONLY: a historical event keeps the heuristic
	// counts it was recorded with (its raw bytes were never retained, so it cannot
	// be re-priced), and only events recorded after calibration carry calibrated
	// counts. A mixed ledger therefore reads HEURISTIC for old rows and CALIBRATED
	// for new ones; the flag reports that the calibrated factor is now in force.
	Calibrated              bool    `json:"calibrated,omitempty"`
	CalibratedBytesPerToken float64 `json:"calibrated_bytes_per_token,omitempty"`
	CalibrationSamples      int     `json:"calibration_samples,omitempty"`

	// Buckets is the saved-tokens trend over the window, oldest bucket first,
	// zero-filled and contiguous so it reads as a real timeseries (not a sparse
	// scatter). Populated only when the caller asks for it (GET /savings?bucket=
	// day|hour); nil/omitted otherwise so the common report stays unchanged. Drives
	// the Metrics-tab trend chart and the CLI sparkline. BucketGranularity names the
	// bucket width the trend was built at.
	Buckets           []Bucket `json:"buckets,omitempty"`
	BucketGranularity string   `json:"bucket_granularity,omitempty"`
	// Samples is a handful of retained provenance pairs (newest first) for the
	// audit view. Populated only when the caller asks (GET /savings?samples=1) and
	// only ever holds events whose samples were retained at record time. omitempty.
	Samples []SamplePair `json:"samples,omitempty"`
}

// Granularity selects the trend's bucket width. The events carry a precise TS,
// so the same ledger can be rolled up by hour (rich intraday detail, the default
// for short windows) or by day (a longer span), without losing resolution the way
// a day-only roll-up does on a fresh install.
const (
	GranularityHour = "hour"
	GranularityDay  = "day"
)

// maxBuckets caps the zero-filled trend so a pathological window (e.g. hour
// granularity over a year) can't allocate an unbounded slice. When the span from
// the first bucket to `until` exceeds the cap, the fill starts at the most recent
// maxBuckets intervals instead of the earliest event.
const maxBuckets = 1500

// Bucket is one interval of the saved-tokens trend (a UTC day or hour) with the
// net tokens saved and event count inside it, the running cumulative saved
// through this bucket (within the window), and the per-feature split. TS is the
// bucket start as unix seconds — the chart x-axis; Date is the human label
// ("2006-01-02" for a day, "2006-01-02 15:00" for an hour). The trend is
// zero-filled across the window, so an idle interval is a real zero, not a gap.
type Bucket struct {
	TS          int64          `json:"ts"`
	Date        string         `json:"date"`
	SavedTokens int            `json:"saved_tokens"`
	Events      int            `json:"events"`
	Cumulative  int            `json:"cumulative"`
	ByFeature   map[string]int `json:"by_feature,omitempty"`
}

// normalizeGranularity coerces an arbitrary bucket value to a known granularity,
// defaulting to day. Empty stays empty (the caller asked for no buckets).
func normalizeGranularity(gran string) string {
	switch gran {
	case GranularityHour:
		return GranularityHour
	default:
		return GranularityDay
	}
}

// bucketStart truncates t to the start of its bucket (UTC) for the granularity.
func bucketStart(t time.Time, gran string) time.Time {
	t = t.UTC()
	if gran == GranularityHour {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// bucketStep is the interval between consecutive bucket starts (UTC has no DST,
// so a day is exactly 24h).
func bucketStep(gran string) time.Duration {
	if gran == GranularityHour {
		return time.Hour
	}
	return 24 * time.Hour
}

// bucketLabel formats a bucket start for display.
func bucketLabel(t time.Time, gran string) string {
	if gran == GranularityHour {
		return t.UTC().Format("2006-01-02 15:00")
	}
	return t.UTC().Format("2006-01-02")
}

// SamplePair is one retained raw-vs-kept provenance sample for the audit view:
// the feature it came from plus the truncated real bytes of each side.
type SamplePair struct {
	Feature    string `json:"feature"`
	RawSample  string `json:"raw_sample"`
	KeptSample string `json:"kept_sample"`
}

// BucketByDay rolls events into per-UTC-day buckets (saved tokens + event count),
// returned oldest day first. Pure — the caller supplies the windowed events. Days
// with no events are absent (the ledger is sparse). Retained for the raw daily
// roll-up; the trend chart uses BucketBy, which zero-fills and accumulates.
func BucketByDay(events []Event) []Bucket {
	byDay := map[string]*Bucket{}
	for _, e := range events {
		start := bucketStart(e.TS, GranularityDay)
		day := start.Format("2006-01-02")
		b := byDay[day]
		if b == nil {
			b = &Bucket{TS: start.Unix(), Date: day}
			byDay[day] = b
		}
		b.SavedTokens += e.Saved
		b.Events++
	}
	out := make([]Bucket, 0, len(byDay))
	for _, b := range byDay {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// BucketBy rolls events into contiguous, zero-filled buckets of the given
// granularity ("hour" or "day"; anything else is treated as day), oldest first.
// Each bucket carries its net saved tokens, event count, the running cumulative
// saved through it, and the per-feature split (for the stacked breakdown). The
// trend spans [start, until]: start is the bucket containing `since`, or the
// first event's bucket when since is zero (all-time); until defaults to now.
// Empty intervals are emitted as real zeros so the line is continuous rather than
// a sparse scatter — the fix for a fresh ledger plotting as a single point. Pure:
// the caller supplies the windowed events and the clock (until), so it is
// unit-testable without wall-clock dependence.
func BucketBy(events []Event, gran string, since, until time.Time) []Bucket {
	gran = normalizeGranularity(gran)
	if until.IsZero() {
		until = time.Now()
	}
	until = until.UTC()

	// Tally events into their bucket-start keys (unix seconds).
	type tally struct {
		saved   int
		events  int
		feature map[string]int
	}
	byStart := map[int64]*tally{}
	var earliest time.Time
	for _, e := range events {
		bs := bucketStart(e.TS, gran)
		if earliest.IsZero() || bs.Before(earliest) {
			earliest = bs
		}
		key := bs.Unix()
		tl := byStart[key]
		if tl == nil {
			tl = &tally{feature: map[string]int{}}
			byStart[key] = tl
		}
		tl.saved += e.Saved
		tl.events++
		tl.feature[e.Feature] += e.Saved
	}

	// Resolve the fill window. An explicit `since` floors it; otherwise the first
	// event does. With neither (no events, all-time) there is nothing to plot.
	var start time.Time
	switch {
	case !since.IsZero():
		start = bucketStart(since, gran)
	case !earliest.IsZero():
		start = earliest
	default:
		return nil
	}
	end := bucketStart(until, gran)
	if end.Before(start) {
		end = start
	}
	step := bucketStep(gran)

	// Bound the slice: slide the start up to show the most recent maxBuckets
	// intervals when the span is too wide.
	if n := int(end.Sub(start)/step) + 1; n > maxBuckets {
		start = end.Add(-time.Duration(maxBuckets-1) * step)
	}

	out := make([]Bucket, 0)
	cum := 0
	for t := start; !t.After(end); t = t.Add(step) {
		b := Bucket{TS: t.Unix(), Date: bucketLabel(t, gran)}
		if tl := byStart[t.Unix()]; tl != nil {
			b.SavedTokens = tl.saved
			b.Events = tl.events
			b.ByFeature = tl.feature
		}
		cum += b.SavedTokens
		b.Cumulative = cum
		out = append(out, b)
	}
	return out
}

// auditSampleLimit caps how many retained provenance pairs the audit view shows —
// a skeptic needs a few real examples, not the whole ledger.
const auditSampleLimit = 5

// collectSamples returns up to auditSampleLimit retained provenance pairs from
// events, newest first (events are oldest-first on disk). Events that carry no
// retained sample are skipped. Pure.
func collectSamples(events []Event) []SamplePair {
	out := make([]SamplePair, 0, auditSampleLimit)
	for i := len(events) - 1; i >= 0 && len(out) < auditSampleLimit; i-- {
		e := events[i]
		if e.RawSample == "" && e.KeptSample == "" {
			continue
		}
		out = append(out, SamplePair{Feature: e.Feature, RawSample: e.RawSample, KeptSample: e.KeptSample})
	}
	return out
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
