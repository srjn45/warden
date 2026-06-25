// Theme (dark-mode) preference. Three settings — 'system' (follow the OS via
// prefers-color-scheme), 'light', and 'dark' — persisted to localStorage and
// reflected as a `data-theme` attribute on <html>. The CSS in app.css keys its
// palette off that attribute (and off the system color-scheme when in 'system'
// mode), so flipping the attribute is all that's needed to retheme the app.

export type Theme = 'system' | 'light' | 'dark';
export type Resolved = 'light' | 'dark';

export const THEME_KEY = 'warden.theme';

// Cycle order for the header toggle: system → light → dark → system.
export const THEME_ORDER: Theme[] = ['system', 'light', 'dark'];

function isTheme(v: unknown): v is Theme {
  return v === 'system' || v === 'light' || v === 'dark';
}

// loadTheme reads the stored preference, defaulting to 'system' when unset,
// invalid, or storage is unavailable (private mode, SSR).
export function loadTheme(): Theme {
  try {
    const raw = localStorage.getItem(THEME_KEY);
    if (isTheme(raw)) return raw;
  } catch { /* storage unavailable */ }
  return 'system';
}

export function saveTheme(t: Theme): void {
  try {
    localStorage.setItem(THEME_KEY, t);
  } catch { /* ignore */ }
}

// systemTheme reads the OS-level preference. Falls back to 'light' when
// matchMedia is missing (older/non-browser environments).
export function systemTheme(): Resolved {
  try {
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  } catch {
    return 'light';
  }
}

// resolveTheme collapses a preference to the concrete light/dark that will
// actually render — useful for picking matching assets (e.g. the wordmark).
export function resolveTheme(t: Theme): Resolved {
  return t === 'system' ? systemTheme() : t;
}

// applyTheme reflects the preference onto <html data-theme=…> so the CSS can
// react. Safe to call before React mounts (and is, via an inline head script).
export function applyTheme(t: Theme): void {
  try {
    document.documentElement.setAttribute('data-theme', t);
  } catch { /* no DOM */ }
}

// nextTheme advances the cycle, wrapping back to the start.
export function nextTheme(t: Theme): Theme {
  const i = THEME_ORDER.indexOf(t);
  return THEME_ORDER[(i + 1) % THEME_ORDER.length];
}

// Presentation helpers for the toggle button.
export const THEME_ICON: Record<Theme, string> = {
  system: '🖥️',
  light: '☀️',
  dark: '🌙',
};

export const THEME_LABEL: Record<Theme, string> = {
  system: 'System',
  light: 'Light',
  dark: 'Dark',
};
