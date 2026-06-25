// The shell has a fixed set of always-present tabs (FIXED_TABS) plus zero or more
// pinned agent tabs (keyed by agent id). `active` is whichever tab is showing —
// a fixed id or a pinned agent id.

// FIXED_TABS are the non-closeable tabs that always exist. Pruning never drops
// the active tab when it is one of these.
export const FIXED_TABS = ['overview', 'cockpit', 'pipelines', 'context', 'archive'] as const;

export function isFixedTab(id: string): boolean {
  return (FIXED_TABS as readonly string[]).includes(id);
}

export interface TabsState {
  pinned: string[]; // agent ids, in open order
  active: string;   // a FIXED_TABS id | <agent id>
}

export type TabsAction =
  | { kind: 'open'; id: string }      // pin (if new) + activate an agent
  | { kind: 'close'; id: string }     // unpin an agent
  | { kind: 'activate'; id: string }  // switch active tab
  | { kind: 'index'; index: number }  // activate the 1-based Nth tab (keyboard 1-9)
  | { kind: 'nav'; delta: number }    // activate `delta` tabs from the active one (j/k)
  | { kind: 'prune'; alive: string[] }; // drop pins not in `alive`

export const initialTabs: TabsState = { pinned: [], active: 'overview' };

// orderedTabs is the left-to-right tab list — the fixed tabs followed by the
// pinned agent tabs in open order. Drives positional (1-9) and relative (j/k)
// keyboard navigation.
export function orderedTabs(s: TabsState): string[] {
  return [...FIXED_TABS, ...s.pinned];
}

// tabByIndex returns the 1-based Nth tab id, or undefined when out of range.
export function tabByIndex(s: TabsState, index: number): string | undefined {
  return orderedTabs(s)[index - 1];
}

// navTab returns the tab `delta` steps from the active one, clamped to the ends
// (no wrap). Falls back to the active id when it isn't in the list.
export function navTab(s: TabsState, delta: number): string {
  const tabs = orderedTabs(s);
  const i = tabs.indexOf(s.active);
  if (i === -1) return s.active;
  return tabs[Math.min(tabs.length - 1, Math.max(0, i + delta))];
}

export function tabsReducer(s: TabsState, a: TabsAction): TabsState {
  switch (a.kind) {
    case 'open': {
      const pinned = s.pinned.includes(a.id) ? s.pinned : [...s.pinned, a.id];
      return { pinned, active: a.id };
    }
    case 'activate':
      return { ...s, active: a.id };
    case 'index': {
      const id = tabByIndex(s, a.index);
      return id ? { ...s, active: id } : s;
    }
    case 'nav':
      return { ...s, active: navTab(s, a.delta) };
    case 'close': {
      const pinned = s.pinned.filter((id) => id !== a.id);
      const active = s.active === a.id ? 'overview' : s.active;
      return { pinned, active };
    }
    case 'prune': {
      const alive = new Set(a.alive);
      const pinned = s.pinned.filter((id) => alive.has(id));
      const active = pinned.includes(s.active) || isFixedTab(s.active)
        ? s.active
        : 'overview';
      return { pinned, active };
    }
    default:
      return s;
  }
}
