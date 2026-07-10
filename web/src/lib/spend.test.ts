import { describe, it, expect } from 'vitest';
import { topAgentSpend, type SpendBucket } from './spend';

const b = (key: string, usd: number, i = 0, o = 0): SpendBucket => ({
  key,
  usd,
  input_tokens: i,
  output_tokens: o,
});

describe('topAgentSpend', () => {
  it('returns empty for null/empty input', () => {
    expect(topAgentSpend(null)).toEqual([]);
    expect(topAgentSpend(undefined)).toEqual([]);
    expect(topAgentSpend([])).toEqual([]);
  });

  it('sorts by cost descending and normalizes fractions to the costliest', () => {
    const out = topAgentSpend([b('a', 1), b('c', 4), b('b', 2)]);
    expect(out.map((x) => x.key)).toEqual(['c', 'b', 'a']);
    expect(out.map((x) => x.frac)).toEqual([1, 0.5, 0.25]);
  });

  it('does not fold when the tail is a single agent (avoids "others (1)")', () => {
    const agents = Array.from({ length: 9 }, (_, i) => b(`a${i}`, 9 - i));
    const out = topAgentSpend(agents, 8);
    expect(out).toHaveLength(9);
    expect(out.some((x) => x.others)).toBe(false);
  });

  it('folds the overflow tail into a single summed "others" row', () => {
    const agents = Array.from({ length: 12 }, (_, i) => b(`a${i}`, 12 - i, 100, 10));
    const out = topAgentSpend(agents, 8);
    expect(out).toHaveLength(9); // 8 top + others
    const others = out[out.length - 1];
    expect(others.others).toBe(true);
    expect(others.key).toBe('others (4)');
    // tail agents a8..a11 cost 4+3+2+1 = 10, tokens 4*100 in / 4*10 out
    expect(others.usd).toBe(10);
    expect(others.input_tokens).toBe(400);
    expect(others.output_tokens).toBe(40);
  });

  it('yields zero-length bars when no agent has positive cost', () => {
    const out = topAgentSpend([b('a', 0), b('b', 0)]);
    expect(out.map((x) => x.frac)).toEqual([0, 0]);
  });
});
