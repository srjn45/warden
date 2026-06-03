import { describe, it, expect } from 'vitest';
import { waitingTransitions } from './notify';
import type { Session } from './types';

const sess = (id: string, status: Session['status']): Session =>
  ({ id, status } as Session);

describe('waitingTransitions', () => {
  it('returns agents that newly entered waiting_for_input', () => {
    const prev = [sess('A-1', 'working'), sess('B-2', 'waiting_for_input')];
    const next = [sess('A-1', 'waiting_for_input'), sess('B-2', 'waiting_for_input')];
    expect(waitingTransitions(prev, next).map((s) => s.id)).toEqual(['A-1']);
  });

  it('treats a brand-new waiting agent as a transition', () => {
    const next = [sess('C-3', 'waiting_for_input')];
    expect(waitingTransitions([], next).map((s) => s.id)).toEqual(['C-3']);
  });

  it('returns nothing when no one is waiting', () => {
    const prev = [sess('A-1', 'working')];
    const next = [sess('A-1', 'idle')];
    expect(waitingTransitions(prev, next)).toEqual([]);
  });
});
