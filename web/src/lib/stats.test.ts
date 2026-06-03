import { describe, it, expect } from 'vitest';
import { deriveFleetStats } from './stats';
import type { Session } from './types';

const sess = (id: string, status: Session['status']): Session =>
  ({ id, status } as Session);

describe('deriveFleetStats', () => {
  it('counts total/busy/waiting/errored buckets', () => {
    const sessions = [
      sess('A-1', 'working'),
      sess('A-2', 'spawning'),
      sess('B-2', 'waiting_for_input'),
      sess('C-3', 'errored'),
      sess('D-4', 'orphaned'),
      sess('E-5', 'idle'),
      sess('F-6', 'done'),
    ];
    expect(deriveFleetStats(sessions)).toEqual({
      total: 7, busy: 2, waiting: 1, errored: 2,
    });
  });

  it('is all-zero for an empty fleet', () => {
    expect(deriveFleetStats([])).toEqual({ total: 0, busy: 0, waiting: 0, errored: 0 });
  });
});
