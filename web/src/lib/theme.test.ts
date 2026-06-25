import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  THEME_KEY, THEME_ORDER, loadTheme, saveTheme, systemTheme,
  resolveTheme, applyTheme, nextTheme, type Theme,
} from './theme';

// Drive systemTheme/resolveTheme by stubbing matchMedia's `matches`.
function stubMatchMedia(prefersDark: boolean | 'throw') {
  if (prefersDark === 'throw') {
    vi.stubGlobal('matchMedia', () => { throw new Error('no matchMedia'); });
    return;
  }
  vi.stubGlobal('matchMedia', (q: string) => ({
    matches: prefersDark && q.includes('dark'),
    media: q,
    addEventListener() {}, removeEventListener() {},
  }));
}

describe('theme preference', () => {
  beforeEach(() => { localStorage.clear(); });
  afterEach(() => { vi.unstubAllGlobals(); });

  it('defaults to system when unset or invalid', () => {
    expect(loadTheme()).toBe('system');
    localStorage.setItem(THEME_KEY, 'neon');
    expect(loadTheme()).toBe('system');
  });

  it('round-trips a saved preference', () => {
    saveTheme('dark');
    expect(localStorage.getItem(THEME_KEY)).toBe('dark');
    expect(loadTheme()).toBe('dark');
  });

  it('cycles system → light → dark → system', () => {
    const seq: Theme[] = [];
    let t: Theme = 'system';
    for (let i = 0; i < 4; i++) { seq.push(t); t = nextTheme(t); }
    expect(seq).toEqual(['system', 'light', 'dark', 'system']);
    expect(THEME_ORDER).toEqual(['system', 'light', 'dark']);
  });

  it('reads the OS preference for system mode', () => {
    stubMatchMedia(true);
    expect(systemTheme()).toBe('dark');
    expect(resolveTheme('system')).toBe('dark');
    stubMatchMedia(false);
    expect(systemTheme()).toBe('light');
    expect(resolveTheme('system')).toBe('light');
  });

  it('resolves explicit choices without consulting the OS', () => {
    stubMatchMedia(true); // OS says dark…
    expect(resolveTheme('light')).toBe('light'); // …but explicit light wins
    expect(resolveTheme('dark')).toBe('dark');
  });

  it('falls back to light when matchMedia is unavailable', () => {
    stubMatchMedia('throw');
    expect(systemTheme()).toBe('light');
  });

  it('reflects the preference onto <html data-theme>', () => {
    applyTheme('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    applyTheme('system');
    expect(document.documentElement.getAttribute('data-theme')).toBe('system');
  });
});
