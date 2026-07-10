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
// per-agent cost bar chart and the daily/weekly totals for the budget headline;
// the remaining fields mirror the wire shape for completeness.
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

// SpendBar is one row of the per-agent cost bar chart: a bucket plus its bar
// length as a 0..1 fraction of the costliest agent, so a large fleet renders as
// a compact, sorted chart instead of an unbounded table. `others` marks the
// synthetic tail row folding every agent past the top-N cutoff.
export interface SpendBar extends SpendBucket {
  frac: number;
  others?: boolean;
}

// topAgentSpend turns the raw by_agent rollup into at most `top` bars sorted by
// cost (highest first), folding any remaining agents into a single "others" row
// whose key reports how many it subsumes. Bar fractions are normalized to the
// costliest agent (which is always a real agent, never the tail) so the longest
// bar fills the track; a report with no positive cost yields zero-length bars.
export function topAgentSpend(agents: SpendBucket[] | null | undefined, top = 8): SpendBar[] {
  const sorted = [...(agents ?? [])].sort((a, b) => b.usd - a.usd);
  if (sorted.length === 0) return [];
  const max = Math.max(sorted[0].usd, 0);
  const norm = (usd: number): number => (max > 0 ? Math.max(usd, 0) / max : 0);
  // Only fold when the tail holds more than one agent — collapsing a single
  // overflow agent into "others (1)" would hide its identity for no gain.
  if (sorted.length <= top + 1) {
    return sorted.map((a) => ({ ...a, frac: norm(a.usd) }));
  }
  const head = sorted.slice(0, top);
  const tail = sorted.slice(top);
  const rest = tail.reduce(
    (acc, a) => {
      acc.usd += a.usd;
      acc.input_tokens += a.input_tokens;
      acc.output_tokens += a.output_tokens;
      return acc;
    },
    { usd: 0, input_tokens: 0, output_tokens: 0 },
  );
  return [
    ...head.map((a) => ({ ...a, frac: norm(a.usd) })),
    {
      key: `others (${tail.length})`,
      usd: rest.usd,
      input_tokens: rest.input_tokens,
      output_tokens: rest.output_tokens,
      frac: norm(rest.usd),
      others: true,
    },
  ];
}
