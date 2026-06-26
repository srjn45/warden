// Pure transforms feeding the Metrics tab's uPlot charts. Everything here is
// I/O-free and unit-tested (metricsSeries.test.ts): MetricsSample[] → aligned
// per-agent series (CPU, RSS) + a fleet-size series, live context samples →
// a client-accumulated per-agent context series, and DayBucket[] →
// tokens-saved bars. All handle empty input and series churn (agents appearing
// and leaving across the window) without producing data uPlot can choke on.
import type { AgentStat, MetricsSample } from './metrics';
import type { DayBucket } from './savings';

// AgentSeries is column-oriented multi-series data: a shared oldest-first time
// axis `t` plus one value column per agent, aligned to `t` with `null` where
// that agent was absent from the sample (uPlot renders nulls as line gaps, so
// agents that come and go don't smear across the window).
export interface AgentSeries {
  t: number[]; // unix seconds, oldest-first
  series: { id: string; values: (number | null)[] }[];
}

const byTakenAt = (a: MetricsSample, b: MetricsSample) =>
  new Date(a.taken_at).getTime() - new Date(b.taken_at).getTime();

// unionIds collects every agent id seen across the (ordered) samples in stable
// first-seen order, so a chart's series set and legend are deterministic.
function unionIds(ordered: MetricsSample[]): string[] {
  const ids: string[] = [];
  const seen = new Set<string>();
  for (const s of ordered) {
    for (const a of s.agents ?? []) {
      if (!seen.has(a.id)) { seen.add(a.id); ids.push(a.id); }
    }
  }
  return ids;
}

// perAgentSeries builds an aligned multi-series view of the history, picking one
// scalar per agent per sample (e.g. cpu_percent, or rss in GiB). Samples arrive
// newest-first from the daemon; output is oldest-first for charting.
export function perAgentSeries(
  samples: MetricsSample[],
  pick: (a: AgentStat) => number,
): AgentSeries {
  const ordered = [...samples].sort(byTakenAt);
  const t = ordered.map((s) => new Date(s.taken_at).getTime() / 1000);
  const ids = unionIds(ordered);
  const series = ids.map((id) => ({
    id,
    values: ordered.map((s) => {
      const a = (s.agents ?? []).find((x) => x.id === id);
      return a ? pick(a) : null;
    }),
  }));
  return { t, series };
}

// cpuSeries: one line per agent, y = cpu_percent.
export function cpuSeries(samples: MetricsSample[]): AgentSeries {
  return perAgentSeries(samples, (a) => a.cpu_percent);
}

// rssSeries: one line per agent, y = resident memory in GiB.
export function rssSeries(samples: MetricsSample[]): AgentSeries {
  return perAgentSeries(samples, (a) => a.rss_bytes / 2 ** 30);
}

// FleetSeries is the single-series fleet-size trend.
export interface FleetSeries {
  t: number[];     // unix seconds, oldest-first
  count: number[]; // system.agent_count aligned to t
}

// fleetSizeSeries: number of agents over time (system.agent_count).
export function fleetSizeSeries(samples: MetricsSample[]): FleetSeries {
  const ordered = [...samples].sort(byTakenAt);
  return {
    t: ordered.map((s) => new Date(s.taken_at).getTime() / 1000),
    count: ordered.map((s) => s.system.agent_count),
  };
}

// SavedSeries is the tokens-saved bar trend. x carries the day timestamp (unix
// seconds, midnight UTC) for a time axis; dates keeps the YYYY-MM-DD label.
export interface SavedSeries {
  x: number[];
  saved: number[];
  dates: string[];
}

// tokensSavedSeries turns the per-day buckets (oldest-first from the daemon)
// into a chartable trend. Empty/absent buckets → empty arrays.
export function tokensSavedSeries(buckets: DayBucket[] | null | undefined): SavedSeries {
  const bs = buckets ?? [];
  return {
    x: bs.map((b) => Date.parse(`${b.date}T00:00:00Z`) / 1000),
    saved: bs.map((b) => b.saved_tokens),
    dates: bs.map((b) => b.date),
  };
}

// ── Context per agent (client-accumulated) ──────────────────────────────────
//
// The metrics history store has no context column, so the Context-per-agent
// series is built in-session from live Session.context_tokens: Dashboard
// samples the fleet on a timer and appends a ContextPoint to a capped ring
// buffer (owned above the tab so it survives tab switches; a full reload starts
// fresh — the documented limitation, spec §4.4 item 3).

// ContextPoint is one timestamped snapshot of every agent's context fill.
export interface ContextPoint {
  t: number;                       // unix seconds
  tokens: Record<string, number>;  // agent id → context_tokens
  states: Record<string, string>;  // agent id → context_state ('' | ok | warning | critical)
}

// ContextSample is the minimal shape appendContextPoint reads off a Session.
export interface ContextSample {
  id: string;
  context_tokens?: number;
  context_state?: string;
}

// CONTEXT_HISTORY_CAP bounds the ring buffer (~20 min at the 5s sample cadence).
export const CONTEXT_HISTORY_CAP = 240;

// appendContextPoint records the current fleet's context fill as one new point,
// dropping the oldest when the buffer is full. Agents without a context_tokens
// reading are skipped; a snapshot with no readable agents is dropped entirely
// (returns prev unchanged) so idle gaps don't pad the buffer with empties.
export function appendContextPoint(
  prev: ContextPoint[],
  sessions: ContextSample[],
  nowSec: number,
  cap = CONTEXT_HISTORY_CAP,
): ContextPoint[] {
  const tokens: Record<string, number> = {};
  const states: Record<string, string> = {};
  for (const s of sessions) {
    if (typeof s.context_tokens === 'number') {
      tokens[s.id] = s.context_tokens;
      states[s.id] = s.context_state ?? '';
    }
  }
  if (Object.keys(tokens).length === 0) return prev;
  const next = [...prev, { t: nowSec, tokens, states }];
  return next.length > cap ? next.slice(next.length - cap) : next;
}

// ContextSeries is an AgentSeries plus each agent's latest context_state, so the
// chart can color the legend by current pressure (ok/warning/critical).
export interface ContextSeries extends AgentSeries {
  stateById: Record<string, string>;
}

// contextSeries aligns the ring buffer into per-agent token lines and resolves
// each agent's most-recent state. Points may arrive in any order; output is
// time-sorted.
export function contextSeries(points: ContextPoint[]): ContextSeries {
  const ordered = [...points].sort((a, b) => a.t - b.t);
  const t = ordered.map((p) => p.t);
  const ids: string[] = [];
  const seen = new Set<string>();
  for (const p of ordered) {
    for (const id of Object.keys(p.tokens)) {
      if (!seen.has(id)) { seen.add(id); ids.push(id); }
    }
  }
  const series = ids.map((id) => ({
    id,
    values: ordered.map((p) => (id in p.tokens ? p.tokens[id] : null)),
  }));
  const stateById: Record<string, string> = {};
  for (const id of ids) {
    for (let i = ordered.length - 1; i >= 0; i--) {
      if (id in ordered[i].states) { stateById[id] = ordered[i].states[id]; break; }
    }
  }
  return { t, series, stateById };
}
