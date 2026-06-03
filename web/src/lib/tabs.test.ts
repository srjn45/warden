import { describe, it, expect } from 'vitest';
import { tabsReducer, initialTabs, type TabsState } from './tabs';

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
});
