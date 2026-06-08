import { describe, it, expect } from 'vitest';
import { contextNamespace, groupContext } from './inspector';
import type { ContextEntry } from './types';

function entry(key: string): ContextEntry {
  return { key, value: 'v', updated_by: 'human', updated_at: '2026-06-08T00:00:00Z' };
}

describe('contextNamespace', () => {
  it('returns the leading dot-segment', () => {
    expect(contextNamespace('pipeline.x.job.output')).toBe('pipeline');
    expect(contextNamespace('global.foo')).toBe('global');
  });
  it('returns (root) for a key with no dot', () => {
    expect(contextNamespace('plainkey')).toBe('(root)');
  });
});

describe('groupContext', () => {
  it('buckets by namespace, sorted by namespace then key', () => {
    const groups = groupContext([
      entry('pipeline.b'),
      entry('global.z'),
      entry('global.a'),
      entry('pipeline.a'),
    ]);
    expect(groups.map((g) => g.namespace)).toEqual(['global', 'pipeline']);
    expect(groups[0].entries.map((e) => e.key)).toEqual(['global.a', 'global.z']);
    expect(groups[1].entries.map((e) => e.key)).toEqual(['pipeline.a', 'pipeline.b']);
  });
  it('does not mutate its input', () => {
    const input = [entry('b.x'), entry('a.y')];
    const snapshot = input.map((e) => e.key);
    groupContext(input);
    expect(input.map((e) => e.key)).toEqual(snapshot);
  });
  it('returns an empty array for no entries', () => {
    expect(groupContext([])).toEqual([]);
  });
});
