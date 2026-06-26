// Mirrors the daemon's savings.Summary wire shape (internal/savings/savings.go),
// the aggregated token reductions warden's lifecycle features earn. Only the
// fields the Metrics tab reads are typed strongly; the rest of the JSON is
// carried for completeness so consumers can grow without another mirror pass.
//
// GET /savings is GATED: when the `savings` config setting is off the daemon
// returns 403 (see internal/daemon/savings_routes.go). getSavings() in api.ts
// surfaces that as an ApiError(403) the Metrics tab turns into an enable hint.

// FeatureSummary is the rolled-up saving for one feature over the window.
export interface FeatureSummary {
  feature: string;
  events: number;
  raw_tokens: number;
  kept_tokens: number;
  saved_tokens: number;
  saved_dollars: number;
}

// Bucket is one interval of the saved-tokens trend (a UTC day or hour) — the
// unit the tokens-saved chart plots. ts is the bucket start (unix seconds, the
// chart x-axis); date is the human label ("2026-06-26" or "2026-06-26 15:00").
// The trend is zero-filled and contiguous, so cumulative runs monotonically and
// idle intervals are real zeros. by_feature is the per-feature split for the
// stacked breakdown. Present only when the request asks for it (GET
// /savings?bucket=day|hour), oldest interval first.
export interface Bucket {
  ts: number;
  date: string;
  saved_tokens: number;
  events: number;
  cumulative: number;
  by_feature?: Record<string, number> | null;
}

// Summary is the whole ledger (over a window) aggregated for display. The
// Metrics tab reads the headline saved_tokens / saved_dollars and the per-day
// buckets; the remaining fields mirror the wire shape for completeness.
export interface Summary {
  since?: string;
  events: number;
  raw_tokens: number;
  kept_tokens: number;
  saved_tokens: number;
  saved_dollars: number;
  reduction_pct: number;
  features: FeatureSummary[] | null;

  context_raw_tokens: number;
  context_kept_tokens: number;
  context_saved_tokens: number;
  context_saved_dollars: number;
  context_reduction_pct: number;

  offloaded_tokens: number;
  offloaded_dollars: number;
  offloaded_events: number;

  measured_spend?: number;
  calibrated?: boolean;

  // Saved-tokens trend (GET /savings?bucket=day|hour), zero-filled and
  // oldest-first. bucket_granularity names the width the trend was built at.
  buckets?: Bucket[] | null;
  bucket_granularity?: string;
}
