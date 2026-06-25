import { describe, it, expect } from 'vitest';
import { tabsReducer, initialTabs, orderedTabs, tabByIndex, navTab, type TabsState } from './tabs';

describe('tabsReducer', () => {
  it('open pins an agent and activates it', () => {
    const s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    expect(s.pinned).toEqual(['A-1']);
    expect(s.active).toBe('A-1');
  });

  it('open is idempotent on the pinned list but re-activates', () => {
    let s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    s = tabsReducer(s, { kind: 'activate', id: 'overview' });
    s = tabsReducer(s, { kind: 'open', id: 'A-1' });
    expect(s.pinned).toEqual(['A-1']);
    expect(s.active).toBe('A-1');
  });

  it('activate switches the active tab without changing pins', () => {
    let s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    s = tabsReducer(s, { kind: 'activate', id: 'cockpit' });
    expect(s.active).toBe('cockpit');
    expect(s.pinned).toEqual(['A-1']);
  });

  it('close removes a pin and falls back to overview when it was active', () => {
    let s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    s = tabsReducer(s, { kind: 'close', id: 'A-1' });
    expect(s.pinned).toEqual([]);
    expect(s.active).toBe('overview');
  });

  it('close keeps the active tab when a different pin is closed', () => {
    let s: TabsState = { pinned: ['A-1', 'B-2'], active: 'B-2' };
    s = tabsReducer(s, { kind: 'close', id: 'A-1' });
    expect(s.pinned).toEqual(['B-2']);
    expect(s.active).toBe('B-2');
  });

  it('prune drops pins for agents that no longer exist', () => {
    let s: TabsState = { pinned: ['A-1', 'B-2'], active: 'A-1' };
    s = tabsReducer(s, { kind: 'prune', alive: ['B-2'] });
    expect(s.pinned).toEqual(['B-2']);
    expect(s.active).toBe('overview');
  });

  it('prune keeps the pipelines fixed tab active', () => {
    const s = { pinned: [], active: 'pipelines' };
    const out = tabsReducer(s, { kind: 'prune', alive: [] });
    expect(out.active).toBe('pipelines');
  });

  it('prune keeps the context fixed tab active', () => {
    const s = { pinned: [], active: 'context' };
    const out = tabsReducer(s, { kind: 'prune', alive: [] });
    expect(out.active).toBe('context');
  });

  it('index activates the Nth tab (1-based) and ignores out-of-range', () => {
    const s: TabsState = { pinned: ['A-1'], active: 'overview' };
    expect(tabsReducer(s, { kind: 'index', index: 1 }).active).toBe('overview');
    expect(tabsReducer(s, { kind: 'index', index: 6 }).active).toBe('A-1');
    expect(tabsReducer(s, { kind: 'index', index: 9 })).toBe(s); // no-op
  });

  it('nav moves through the tab list and clamps at the ends', () => {
    const s: TabsState = { pinned: ['A-1'], active: 'overview' };
    expect(tabsReducer(s, { kind: 'nav', delta: 1 }).active).toBe('cockpit');
    expect(tabsReducer(s, { kind: 'nav', delta: -1 }).active).toBe('overview'); // clamped
    const last: TabsState = { pinned: ['A-1'], active: 'A-1' };
    expect(tabsReducer(last, { kind: 'nav', delta: 1 }).active).toBe('A-1'); // clamped
    expect(tabsReducer(last, { kind: 'nav', delta: -1 }).active).toBe('archive');
  });
});

describe('tab list helpers', () => {
  it('orderedTabs is fixed tabs followed by pins in open order', () => {
    expect(orderedTabs({ pinned: ['A-1', 'B-2'], active: 'overview' })).toEqual([
      'overview', 'cockpit', 'pipelines', 'context', 'archive', 'A-1', 'B-2',
    ]);
  });

  it('tabByIndex is 1-based and undefined out of range', () => {
    const s: TabsState = { pinned: ['A-1'], active: 'overview' };
    expect(tabByIndex(s, 1)).toBe('overview');
    expect(tabByIndex(s, 6)).toBe('A-1');
    expect(tabByIndex(s, 7)).toBeUndefined();
  });

  it('navTab falls back to active when it is not in the list', () => {
    expect(navTab({ pinned: [], active: 'ghost' }, 1)).toBe('ghost');
  });
});
