import { describe, it, expect, beforeEach } from 'vitest';
import {
  parseRoute, routeToPath, navigate, redirectRootToDefault, currentRoute,
  DEFAULT_ROUTE, DEFAULT_PATH, FIXED_ROUTE_KINDS, type Route,
} from './router';

describe('parseRoute', () => {
  it('maps each fixed path to its route', () => {
    for (const kind of FIXED_ROUTE_KINDS) {
      expect(parseRoute(`/${kind}`)).toEqual({ kind });
    }
  });

  it('redirects `/` to the default (cockpit) route', () => {
    expect(parseRoute('/')).toEqual(DEFAULT_ROUTE);
    expect(DEFAULT_ROUTE).toEqual({ kind: 'cockpit' });
  });

  it('parses /tui as the full-screen route (not a fixed tab)', () => {
    expect(parseRoute('/tui')).toEqual({ kind: 'tui' });
    expect(parseRoute('/tui/')).toEqual({ kind: 'tui' });
    expect(routeToPath({ kind: 'tui' })).toBe('/tui');
    expect(FIXED_ROUTE_KINDS).not.toContain('tui');
  });

  it('parses agent routes and decodes the id', () => {
    expect(parseRoute('/agent/A-1')).toEqual({ kind: 'agent', id: 'A-1' });
    expect(parseRoute('/agent/feat%2Fx')).toEqual({ kind: 'agent', id: 'feat/x' });
  });

  it('falls back to cockpit for unknown paths', () => {
    expect(parseRoute('/nope')).toEqual(DEFAULT_ROUTE);
    expect(parseRoute('/context')).toEqual(DEFAULT_ROUTE); // no longer a route
    expect(parseRoute('/agent')).toEqual(DEFAULT_ROUTE);   // missing id
    expect(parseRoute('/cockpit/extra')).toEqual(DEFAULT_ROUTE);
  });

  it('tolerates a trailing slash', () => {
    expect(parseRoute('/metrics/')).toEqual({ kind: 'metrics' });
  });
});

describe('routeToPath', () => {
  it('is the inverse of parseRoute for fixed routes', () => {
    for (const kind of FIXED_ROUTE_KINDS) {
      const r: Route = { kind };
      expect(routeToPath(r)).toBe(`/${kind}`);
      expect(parseRoute(routeToPath(r))).toEqual(r);
    }
  });

  it('round-trips agent routes (including ids needing encoding)', () => {
    for (const id of ['A-1', 'feat/x', 'a b']) {
      const r: Route = { kind: 'agent', id };
      expect(parseRoute(routeToPath(r))).toEqual(r);
    }
  });
});

describe('history integration', () => {
  beforeEach(() => {
    window.history.replaceState({}, '', '/');
  });

  it('redirectRootToDefault replaces `/` with /cockpit once', () => {
    expect(window.location.pathname).toBe('/');
    expect(redirectRootToDefault()).toBe(true);
    expect(window.location.pathname).toBe(DEFAULT_PATH);
    // No re-fire once we're already off `/`.
    expect(redirectRootToDefault()).toBe(false);
    expect(window.location.pathname).toBe(DEFAULT_PATH);
  });

  it('navigate pushes the route path and currentRoute reflects it', () => {
    navigate({ kind: 'metrics' });
    expect(window.location.pathname).toBe('/metrics');
    expect(currentRoute()).toEqual({ kind: 'metrics' });

    navigate({ kind: 'agent', id: 'A-1' });
    expect(window.location.pathname).toBe('/agent/A-1');
    expect(currentRoute()).toEqual({ kind: 'agent', id: 'A-1' });
  });
});
