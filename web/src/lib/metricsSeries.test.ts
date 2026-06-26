import { describe, it, expect } from 'vitest';
import type { MetricsSample, AgentStat } from './metrics';
import type { DayBucket } from './savings';
import {
  cpuSeries, rssSeries, fleetSizeSeries, tokensSavedSeries,
  appendContextPoint, contextSeries, perAgentSeries,
  type ContextPoint,
} from './metricsSeries';

const agent = (id: string, cpu: number, rss: number): AgentStat => ({
  id, status: 'busy', paneable: true, rss_bytes: rss, cpu_percent: cpu, proc_count: 1, uptime_sec: 10,
});

const sample = (t: string, agents: AgentStat[], count = agents.length): MetricsSample => ({
  taken_at: t,
  system: {
    total_bytes: 0, used_bytes: 0, free_bytes: 0, wired_bytes: 0, compressed_bytes: 0,
    swap_used_bytes: 0, pressure_level: 'normal', agent_count: count, attributed_rss_bytes: 0,
  },
  agents,
  daemon: { rss_bytes: 0, goroutines: 0, open_fds: 0 },
});

describe('perAgentSeries / cpuSeries', () => {
  it('aligns one column per agent, oldest-first, with nulls for churn', () => {
    // daemon returns newest-first; agent b only present in the newer sample.
    const history = [
      sample('2026-06-26T10:00:05Z', [agent('a', 20, 0), agent('b', 50, 0)]),
      sample('2026-06-26T10:00:00Z', [agent('a', 10, 0)]),
    ];
    const s = cpuSeries(history);
    expect(s.t).toHaveLength(2);
    expect(s.t[0]).toBeLessThan(s.t[1]); // oldest first
    expect(s.series.map((x) => x.id)).toEqual(['a', 'b']);
    expect(s.series[0].values).toEqual([10, 20]); // a present in both
    expect(s.series[1].values).toEqual([null, 50]); // b absent in the oldest → null gap
  });

  it('returns empty arrays for empty input', () => {
    const s = cpuSeries([]);
    expect(s.t).toEqual([]);
    expect(s.series).toEqual([]);
  });

  it('lets the picker select any agent scalar', () => {
    const s = perAgentSeries([sample('2026-06-26T10:00:00Z', [agent('a', 0, 7)])], (a) => a.proc_count);
    expect(s.series[0].values).toEqual([1]);
  });
});

describe('rssSeries', () => {
  it('converts rss_bytes to GiB', () => {
    const s = rssSeries([sample('2026-06-26T10:00:00Z', [agent('a', 0, 2 ** 30)])]);
    expect(s.series[0].values[0]).toBeCloseTo(1);
  });
});

describe('fleetSizeSeries', () => {
  it('extracts agent_count oldest-first', () => {
    const fs = fleetSizeSeries([
      sample('2026-06-26T10:00:05Z', [], 3),
      sample('2026-06-26T10:00:00Z', [], 1),
    ]);
    expect(fs.count).toEqual([1, 3]);
  });

  it('handles empty input', () => {
    expect(fleetSizeSeries([])).toEqual({ t: [], count: [] });
  });
});

describe('tokensSavedSeries', () => {
  it('maps buckets to x/saved/dates', () => {
    const buckets: DayBucket[] = [
      { date: '2026-06-25', saved_tokens: 100, events: 2 },
      { date: '2026-06-26', saved_tokens: 250, events: 5 },
    ];
    const s = tokensSavedSeries(buckets);
    expect(s.saved).toEqual([100, 250]);
    expect(s.dates).toEqual(['2026-06-25', '2026-06-26']);
    expect(s.x[0]).toBe(Date.parse('2026-06-25T00:00:00Z') / 1000);
  });

  it('handles null/undefined buckets', () => {
    expect(tokensSavedSeries(null)).toEqual({ x: [], saved: [], dates: [] });
    expect(tokensSavedSeries(undefined)).toEqual({ x: [], saved: [], dates: [] });
  });
});

describe('appendContextPoint', () => {
  it('records readable agents and skips agents without context_tokens', () => {
    const next = appendContextPoint([], [
      { id: 'a', context_tokens: 100, context_state: 'ok' },
      { id: 'b' }, // no reading → skipped
    ], 1000);
    expect(next).toHaveLength(1);
    expect(next[0]).toEqual({ t: 1000, tokens: { a: 100 }, states: { a: 'ok' } });
  });

  it('drops a snapshot with no readable agents (no empty padding)', () => {
    const prev: ContextPoint[] = [{ t: 1, tokens: { a: 1 }, states: { a: '' } }];
    expect(appendContextPoint(prev, [{ id: 'b' }], 2)).toBe(prev);
  });

  it('caps the ring buffer, dropping the oldest', () => {
    let buf: ContextPoint[] = [];
    for (let i = 0; i < 5; i++) buf = appendContextPoint(buf, [{ id: 'a', context_tokens: i }], i, 3);
    expect(buf).toHaveLength(3);
    expect(buf.map((p) => p.t)).toEqual([2, 3, 4]); // oldest two dropped
  });
});

describe('contextSeries', () => {
  it('aligns per-agent token columns and resolves latest state', () => {
    const points: ContextPoint[] = [
      { t: 2, tokens: { a: 20, b: 5 }, states: { a: 'warning', b: 'ok' } },
      { t: 1, tokens: { a: 10 }, states: { a: 'ok' } }, // out of order; b absent
    ];
    const cs = contextSeries(points);
    expect(cs.t).toEqual([1, 2]); // sorted
    expect(cs.series.map((s) => s.id)).toEqual(['a', 'b']);
    expect(cs.series[0].values).toEqual([10, 20]);
    expect(cs.series[1].values).toEqual([null, 5]); // b absent at t=1
    expect(cs.stateById).toEqual({ a: 'warning', b: 'ok' }); // latest per agent
  });

  it('handles empty input', () => {
    expect(contextSeries([])).toEqual({ t: [], series: [], stateById: {} });
  });
});
