import { describe, it, expect } from 'vitest';
import { splitByTier, outcomeSegments, fmtMs, classLabel, type FBKindTier } from './fastbrain';

const h = { bounds_ms: [], counts: [], count: 0, sum_ms: 0, p50_ms: 0, p95_ms: 0 };
const k = (kind: string, tier: string, attempts: number): FBKindTier => ({
  kind, tier, class: 3, attempts, outcomes: {}, cached: 0, coalesced: 0, fallbacks: 0,
  fail_open: 0, cancel_acknowledged: 0, cancel_abandoned: 0, queue_wait: h, run_time: h,
});

describe('fastbrain helpers', () => {
  it('splits by tier, drops idle kinds, sorts by attempts', () => {
    const r = splitByTier([k('a', 'fast', 1), k('b', 'thinking', 5), k('c', 'fast', 0), k('d', 'fast', 9)]);
    expect(r.fast.map((x) => x.kind)).toEqual(['d', 'a']);
    expect(r.thinking.map((x) => x.kind)).toEqual(['b']);
  });
  it('builds outcome segments', () => {
    const s = outcomeSegments({ ok: 3, failed: 1, none: 0 });
    expect(s.map((x) => x.name)).toEqual(['ok', 'failed']);
    expect(s[0].frac).toBeCloseTo(0.75);
    expect(outcomeSegments({})).toEqual([]);
  });
  it('formats', () => {
    expect(fmtMs(250)).toBe('250ms');
    expect(fmtMs(2500)).toBe('2.5s');
    expect(classLabel(1)).toBe('P1 safety');
    expect(classLabel(9)).toBe('P9');
  });
});
