// Mirrors the daemon's spend.Report wire shape (internal/spend/report.go): the
// measured Claude spend priced per model into dollars and rolled up per-agent,
// per-repo, and per-day, plus the daily/weekly totals the budget gate enforces.
//
// GET /spend is GATED by the same `savings` config setting the ledger uses: when
// it is off the daemon returns 403 (see internal/daemon/spend_routes.go), which
// getSpend() in api.ts surfaces as an ApiError(403) the Metrics tab turns into an
// enable hint rather than an empty card.

// SpendBucket is one rollup row — an agent, repo, or day — with the billed tokens
// behind it and the priced dollar figure.
export interface SpendBucket {
  key: string;
  input_tokens: number;
  output_tokens: number;
  usd: number;
}

// Report is the whole cost rollup. The Metrics tab reads by_agent for the live
// per-agent cost table and the daily/weekly totals for the budget headline; the
// remaining fields mirror the wire shape for completeness.
export interface Report {
  total_usd: number;
  input_tokens: number;
  output_tokens: number;
  by_agent: SpendBucket[] | null;
  by_repo: SpendBucket[] | null;
  by_day: SpendBucket[] | null;
  daily_usd: number;
  weekly_usd: number;
}
