// The shell has three fixed tabs ('overview', 'cockpit', 'pipelines') that always
// exist, plus zero or more pinned agent tabs (keyed by agent id). `active` is
// whichever tab is showing — a fixed id or a pinned agent id.

export interface TabsState {
  pinned: string[]; // agent ids, in open order
  active: string;   // 'overview' | 'cockpit' | <agent id>
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
      const active = pinned.includes(s.active) || s.active === 'overview' || s.active === 'cockpit' || s.active === 'pipelines'
        ? s.active
        : 'overview';
      return { pinned, active };
    }
    default:
      return s;
  }
}
