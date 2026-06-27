// Tiny hash-free client-side router built on the History API — no router
// dependency. The app is a single Astro-mounted React SPA; this maps the URL
// path to a `Route` and back, so tabs become real, deep-linkable URLs with
// working Back/Forward.
//
// Routes:
//   /cockpit /pipelines /metrics /archive /others  → fixed tabs
//   /tui                                            → the full-screen cockpit
//                                                     (launched from the top bar,
//                                                     not a tab)
//   /agent/<id>                                     → a pinned agent pane
//   anything else                                   → cockpit (default)
//   /                                               → redirects to /cockpit
//
// `/context` is intentionally NOT a route — Context & Messages is a header
// overlay now, not a navigable surface.

import { useEffect, useState } from 'react';

// FIXED_ROUTE_KINDS is the canonical left-to-right order of the always-present
// tabs. Mirrored by FIXED_TABS in lib/tabs.ts (kept in sync deliberately).
// Others is the catch-all and sits last, after the purpose-built tabs.
// `tui` is deliberately NOT here — it is a full-screen route launched from the
// top bar, with no tab and no slot in 1-9 / j-k navigation.
export const FIXED_ROUTE_KINDS = ['cockpit', 'pipelines', 'metrics', 'archive', 'others'] as const;
export type FixedRouteKind = (typeof FIXED_ROUTE_KINDS)[number];

export type Route =
  | { kind: FixedRouteKind }
  | { kind: 'tui' }
  | { kind: 'agent'; id: string };

// The canonical home path; `/` redirects here on first load.
export const DEFAULT_PATH = '/cockpit';
export const DEFAULT_ROUTE: Route = { kind: 'cockpit' };

// Internal event fired by navigate() so the useRoute() hook can re-render
// without a real popstate (pushState/replaceState don't emit one).
const NAV_EVENT = 'warden:navigate';

function isFixedKind(s: string): s is FixedRouteKind {
  return (FIXED_ROUTE_KINDS as readonly string[]).includes(s);
}

// parseRoute maps a pathname to a Route. Unknown paths fall back to cockpit.
export function parseRoute(pathname: string): Route {
  // Normalize: strip a single trailing slash (but keep "/"), then split.
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, '') : pathname;
  const segs = path.split('/').filter(Boolean); // ['agent','A-1'] etc.
  if (segs.length === 0) return DEFAULT_ROUTE; // "/" → cockpit
  if (segs[0] === 'agent' && segs.length >= 2) {
    return { kind: 'agent', id: decodeURIComponent(segs.slice(1).join('/')) };
  }
  if (segs.length === 1 && segs[0] === 'tui') return { kind: 'tui' };
  if (segs.length === 1 && isFixedKind(segs[0])) return { kind: segs[0] };
  return DEFAULT_ROUTE;
}

// routeToPath is the inverse of parseRoute — for building hrefs and pushState.
export function routeToPath(route: Route): string {
  if (route.kind === 'agent') return `/agent/${encodeURIComponent(route.id)}`;
  return `/${route.kind}`;
}

// navigate pushes a new history entry and notifies subscribers. A no-op when
// the target path already matches the current location (avoids dupe entries).
export function navigate(route: Route): void {
  const path = routeToPath(route);
  if (typeof window === 'undefined') return;
  if (window.location.pathname !== path) {
    window.history.pushState({}, '', path);
  }
  window.dispatchEvent(new Event(NAV_EVENT));
}

// redirectRootToDefault runs ONCE on initial load: if we're at `/`, replace it
// with /cockpit (a replace, not a push, so Back doesn't bounce). Returns true
// when it redirected. Guarded by caller to run only on first mount.
export function redirectRootToDefault(): boolean {
  if (typeof window === 'undefined') return false;
  if (window.location.pathname === '/') {
    window.history.replaceState({}, '', DEFAULT_PATH);
    return true;
  }
  return false;
}

// currentRoute reads the live location. Safe to call during SSR (returns home).
export function currentRoute(): Route {
  if (typeof window === 'undefined') return DEFAULT_ROUTE;
  return parseRoute(window.location.pathname);
}

// useRoute subscribes to history changes (Back/Forward via popstate, and our
// navigate() event) and returns the current Route. Replaces the old reducer +
// localStorage active-tab state in Dashboard.
export function useRoute(): Route {
  const [route, setRoute] = useState<Route>(currentRoute);
  useEffect(() => {
    const sync = () => setRoute(currentRoute());
    window.addEventListener('popstate', sync);
    window.addEventListener(NAV_EVENT, sync);
    // Re-sync once on mount in case the path changed between render and effect
    // (e.g. the one-time root redirect).
    sync();
    return () => {
      window.removeEventListener('popstate', sync);
      window.removeEventListener(NAV_EVENT, sync);
    };
  }, []);
  return route;
}
