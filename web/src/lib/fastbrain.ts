// Types + pure helpers for the Fast-Brain / Thinking-Brain metrics card. The
// shapes mirror GET /fastbrain/metrics and /fastbrain/decisions (snake_case).
// Everything here is bounded vocabulary — no prompts, paths or agent IDs.

export interface FBHistogram {
  bounds_ms: number[];
  counts: number[];
  count: number;
  sum_ms: number;
  p50_ms: number;
  p95_ms: number;
}

export interface FBKindTier {
  kind: string;
  tier: 'fast' | 'thinking' | string;
  class: number;
  attempts: number;
  outcomes: Record<string, number>;
  shed?: Record<string, number>;
  cached: number;
  coalesced: number;
  fallbacks: number;
  fail_open: number;
  cancel_acknowledged: number;
  cancel_abandoned: number;
  queue_wait: FBHistogram;
  run_time: FBHistogram;
}

export interface FBRunner {
  provider: string;
  model?: string;
  tier: string;
  calls: number;
  ok: number;
  failed: number;
  run_sum_ms: number;
}

export interface FBCircuit {
  provider: string;
  state: 'closed' | 'open' | 'half_open' | string;
  consecutive_failures: number;
  last_failure?: string;
  retry_at?: string;
  opens: number;
}

export interface FBControl {
  kind: string;
  class: number;
  paused: boolean;
  until?: string;
  since?: string;
  source?: 'operator' | 'config' | string;
}

export interface FBQueue { class: number; active: number; queued: number }

export interface FBTelemetry {
  now: string;
  max_concurrent: number;
  active: number;
  admitted: number;
  coalesced: number;
  cache_hits: number;
  cache_items: number;
  preempted: number;
  shed: Record<string, number>;
  queues: FBQueue[];
  abandoned_total: number;
  abandoned_live: number;
  kinds: FBKindTier[];
  runners: FBRunner[];
  circuits: FBCircuit[];
  controls: FBControl[];
  total_decisions: number;
}

export interface FBDecision {
  time: string;
  kind: string;
  tier: string;
  class: number;
  outcome: string;
  final_action: string;
  reason?: string;
  provider?: string;
  model?: string;
  queue_wait_ms: number;
  run_ms: number;
  coalesced?: boolean;
  cached?: boolean;
  fallback?: boolean;
  attempts?: number;
  cancel?: string;
}

export const CLASS_LABELS: Record<number, string> = {
  1: 'P1 safety',
  2: 'P2 interactive',
  3: 'P3 background',
  4: 'P4 best-effort',
};

export function classLabel(c: number): string {
  return CLASS_LABELS[c] ?? `P${c}`;
}

// splitByTier groups per-kind rows into the Fast and Thinking views, dropping
// kinds that never ran so the card shows only live decision points.
export function splitByTier(kinds: FBKindTier[]): { fast: FBKindTier[]; thinking: FBKindTier[] } {
  const fast: FBKindTier[] = [];
  const thinking: FBKindTier[] = [];
  for (const k of kinds) {
    if (k.attempts === 0 && !(k.cached || k.coalesced)) continue;
    (k.tier === 'thinking' ? thinking : fast).push(k);
  }
  const byAttempts = (a: FBKindTier, b: FBKindTier) => b.attempts - a.attempts;
  return { fast: fast.sort(byAttempts), thinking: thinking.sort(byAttempts) };
}

// outcomeSegments turns an outcome map into stable, ordered bar segments with
// fractions of the total; zero-count outcomes are omitted.
export function outcomeSegments(outcomes: Record<string, number>): { name: string; n: number; frac: number }[] {
  const total = Object.values(outcomes).reduce((a, b) => a + b, 0);
  if (total === 0) return [];
  return Object.entries(outcomes)
    .filter(([, n]) => n > 0)
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([name, n]) => ({ name, n, frac: n / total }));
}

export function fmtMs(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(ms < 10000 ? 1 : 0)}s`;
}
