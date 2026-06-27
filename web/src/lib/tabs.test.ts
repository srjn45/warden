import { describe, it, expect, beforeEach } from 'vitest';
import {
  FIXED_TABS, isFixedTab, orderedRoutes, routeIndex, routeByIndex, navRoute,
  openPin, closePin, prunePins, loadPinned, savePinned,
} from './tabs';
import type { Route } from './router';

describe('fixed tabs', () => {
  it('is the new route list (no context, no overview, no tui — that is full-screen)', () => {
    expect(FIXED_TABS).toEqual(['cockpit', 'pipelines', 'metrics', 'archive', 'others']);
    expect(isFixedTab('cockpit')).toBe(true);
    expect(isFixedTab('others')).toBe(true);
    expect(isFixedTab('metrics')).toBe(true);
    expect(isFixedTab('context')).toBe(false);
    expect(isFixedTab('overview')).toBe(false);
  });
});

describe('ordered routes & keyboard navigation', () => {
  it('orderedRoutes is fixed routes followed by pins in open order', () => {
    expect(orderedRoutes(['A-1', 'B-2'])).toEqual([
      { kind: 'cockpit' }, { kind: 'pipelines' }, { kind: 'metrics' },
      { kind: 'archive' }, { kind: 'others' },
      { kind: 'agent', id: 'A-1' }, { kind: 'agent', id: 'B-2' },
    ]);
  });

  it('routeByIndex is 1-based and undefined out of range', () => {
    expect(routeByIndex(['A-1'], 1)).toEqual({ kind: 'cockpit' });
    expect(routeByIndex(['A-1'], 2)).toEqual({ kind: 'pipelines' });
    expect(routeByIndex(['A-1'], 6)).toEqual({ kind: 'agent', id: 'A-1' });
    expect(routeByIndex(['A-1'], 7)).toBeUndefined();
  });

  it('routeIndex finds the current route, -1 when absent', () => {
    expect(routeIndex(['A-1'], { kind: 'others' })).toBe(4);
    expect(routeIndex(['A-1'], { kind: 'agent', id: 'A-1' })).toBe(5);
    expect(routeIndex(['A-1'], { kind: 'agent', id: 'ghost' })).toBe(-1);
  });

  it('navRoute moves through the list and clamps at the ends', () => {
    expect(navRoute(['A-1'], { kind: 'cockpit' }, 1)).toEqual({ kind: 'pipelines' });
    expect(navRoute(['A-1'], { kind: 'cockpit' }, -1)).toEqual({ kind: 'cockpit' }); // clamped
    const last: Route = { kind: 'agent', id: 'A-1' };
    expect(navRoute(['A-1'], last, 1)).toEqual(last); // clamped
    expect(navRoute(['A-1'], last, -1)).toEqual({ kind: 'others' });
  });

  it('navRoute falls back to the current route when it is not in the list', () => {
    const ghost: Route = { kind: 'agent', id: 'ghost' };
    expect(navRoute([], ghost, 1)).toEqual(ghost);
  });
});

describe('pinned-list operations', () => {
  it('openPin adds new ids and is idempotent', () => {
    expect(openPin([], 'A-1')).toEqual(['A-1']);
    expect(openPin(['A-1'], 'A-1')).toEqual(['A-1']);
    expect(openPin(['A-1'], 'B-2')).toEqual(['A-1', 'B-2']);
  });

  it('closePin removes an id', () => {
    expect(closePin(['A-1', 'B-2'], 'A-1')).toEqual(['B-2']);
    expect(closePin(['A-1'], 'ghost')).toEqual(['A-1']);
  });

  it('prunePins drops ids that are no longer alive', () => {
    expect(prunePins(['A-1', 'B-2'], ['B-2'])).toEqual(['B-2']);
    expect(prunePins(['A-1'], [])).toEqual([]);
  });
});

describe('pinned persistence', () => {
  beforeEach(() => localStorage.clear());

  it('round-trips through localStorage', () => {
    savePinned(['A-1', 'B-2']);
    expect(loadPinned()).toEqual(['A-1', 'B-2']);
  });

  it('returns empty when nothing is stored', () => {
    expect(loadPinned()).toEqual([]);
  });

  it('migrates an old blob with a stray active field (reads only pinned)', () => {
    localStorage.setItem('warden.tabs', JSON.stringify({ pinned: ['A-1'], active: 'overview' }));
    expect(loadPinned()).toEqual(['A-1']);
  });

  it('tolerates a corrupt blob', () => {
    localStorage.setItem('warden.tabs', '{not json');
    expect(loadPinned()).toEqual([]);
  });
});
