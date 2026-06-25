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
  | { kind: 'prune'; alive: string[] }; // drop pins not in `alive`

export const initialTabs: TabsState = { pinned: [], active: 'overview' };

export function tabsReducer(s: TabsState, a: TabsAction): TabsState {
  switch (a.kind) {
    case 'open': {
      const pinned = s.pinned.includes(a.id) ? s.pinned : [...s.pinned, a.id];
      return { pinned, active: a.id };
    }
    case 'activate':
      return { ...s, active: a.id };
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
