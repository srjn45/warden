import { describe, it, expect } from 'vitest';
import { needsAttention } from './attention';
import type { Session } from './types';

const sess = (id: string, status: Session['status']): Session =>
  ({ id, status } as Session);

describe('needsAttention', () => {
  it('selects waiting_for_input, errored, and orphaned agents', () => {
    const sessions = [
      sess('A-1', 'working'),
      sess('B-2', 'waiting_for_input'),
      sess('C-3', 'errored'),
      sess('D-4', 'idle'),
      sess('E-5', 'orphaned'),
    ];
    expect(needsAttention(sessions).map((s) => s.id)).toEqual(['B-2', 'C-3', 'E-5']);
  });

  it('returns an empty array when nothing needs attention', () => {
    expect(needsAttention([sess('A-1', 'working')])).toEqual([]);
  });
});
