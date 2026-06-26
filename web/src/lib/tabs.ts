// The shell has a fixed set of always-present tabs (FIXED_TABS) plus zero or more
// pinned agent tabs (keyed by agent id). The *active* tab is no longer stored —
// it is derived from the URL (see lib/router.ts). This module owns only the
// pinned-agent list (which agent panes show in the bar) plus the ordered-tab
// helpers that drive positional (1-9) and relative (j/k) keyboard navigation,
// now keyed off routes rather than a reducer.

import type { Route } from './router';
import { FIXED_ROUTE_KINDS, routeToPath } from './router';

// FIXED_TABS are the non-closeable tabs that always exist, in left-to-right
// order. Mirrors FIXED_ROUTE_KINDS in lib/router.ts.
export const FIXED_TABS = FIXED_ROUTE_KINDS;

export function isFixedTab(id: string): boolean {
  return (FIXED_TABS as readonly string[]).includes(id);
}

// orderedRoutes is the left-to-right tab list as routes — the fixed tabs
// followed by the pinned agent tabs in open order. Drives positional (1-9) and
// relative (j/k) keyboard navigation.
export function orderedRoutes(pinned: string[]): Route[] {
  return [
    ...FIXED_TABS.map((kind): Route => ({ kind })),
    ...pinned.map((id): Route => ({ kind: 'agent', id })),
  ];
}

// routeIndex returns the 0-based position of `route` in the ordered tab list,
// or -1 when it isn't present (e.g. a stale /agent/<deadid>).
export function routeIndex(pinned: string[], route: Route): number {
  const path = routeToPath(route);
  return orderedRoutes(pinned).findIndex((r) => routeToPath(r) === path);
}

// routeByIndex returns the 1-based Nth tab route, or undefined when out of
// range (keyboard 1-9).
export function routeByIndex(pinned: string[], index: number): Route | undefined {
  return orderedRoutes(pinned)[index - 1];
}

// navRoute returns the route `delta` steps from `route`, clamped to the ends
// (no wrap). Falls back to `route` itself when it isn't in the list (j/k).
export function navRoute(pinned: string[], route: Route, delta: number): Route {
  const routes = orderedRoutes(pinned);
  const i = routeIndex(pinned, route);
  if (i === -1) return route;
  return routes[Math.min(routes.length - 1, Math.max(0, i + delta))];
}

// --- pinned-agent list operations (pure) -----------------------------------

// openPin adds an agent id to the pinned list if not already present.
export function openPin(pinned: string[], id: string): string[] {
  return pinned.includes(id) ? pinned : [...pinned, id];
}

// closePin removes an agent id from the pinned list.
export function closePin(pinned: string[], id: string): string[] {
  return pinned.filter((p) => p !== id);
}

// prunePins drops pins for agents that are no longer alive.
export function prunePins(pinned: string[], alive: string[]): string[] {
  const set = new Set(alive);
  return pinned.filter((p) => set.has(p));
}

// --- pinned-agent persistence ----------------------------------------------
//
// We persist only the pinned list now (active is URL-driven). Old blobs wrote
// the whole TabsState (`{pinned, active}`) under the same key — reading just
// `.pinned` migrates them transparently; a stray `active:'overview'` is ignored.

const TABS_KEY = 'warden.tabs';

export function loadPinned(): string[] {
  try {
    const raw = localStorage.getItem(TABS_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as { pinned?: unknown };
    if (Array.isArray(parsed?.pinned)) {
      return parsed.pinned.filter((x): x is string => typeof x === 'string');
    }
  } catch { /* corrupt / unavailable storage */ }
  return [];
}

export function savePinned(pinned: string[]): void {
  try {
    localStorage.setItem(TABS_KEY, JSON.stringify({ pinned }));
  } catch { /* ignore */ }
}
