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

// DayBucket is one UTC calendar day's saved-tokens roll-up — the unit the
// tokens-saved trend plots. date is YYYY-MM-DD (UTC). Present only when the
// request asks for it (GET /savings?bucket=day), oldest day first.
export interface DayBucket {
  date: string;
  saved_tokens: number;
  events: number;
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

  // Per-day saved-tokens trend (GET /savings?bucket=day), oldest day first.
  buckets?: DayBucket[] | null;
}
