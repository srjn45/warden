// Global keyboard shortcuts for the dashboard shell. Until now only individual
// modals handled their own local keydowns; this is the app-wide layer. The
// key→action mapping lives here as a pure function so it can be unit-tested
// without a DOM — the wiring (attaching the window listener, performing the
// actions) lives in Dashboard, and the `?` help overlay renders BINDINGS.

export type Shortcut =
  | { kind: 'help' }                 // ? — toggle the help overlay
  | { kind: 'new' }                  // n — open the New-agent modal
  | { kind: 'filter' }               // / — focus the agent filter
  | { kind: 'refresh' }              // r — refetch the fleet
  | { kind: 'close' }                // Esc — close a modal / overlay / blur a field
  | { kind: 'tab'; index: number }   // 1-9 — jump to the Nth tab
  | { kind: 'nav'; delta: 1 | -1 };  // j / k — next / previous tab

// Bindings as shown in the `?` help overlay; this array is the display order.
export interface Binding { keys: string; label: string; }
export const BINDINGS: Binding[] = [
  { keys: '?', label: 'Show / hide this help' },
  { keys: 'n', label: 'New agent' },
  { keys: '/', label: 'Focus the agent filter' },
  { keys: 'r', label: 'Refresh the fleet' },
  { keys: '1–9', label: 'Jump to tab by position' },
  { keys: 'j / k', label: 'Next / previous tab' },
  { keys: 'Esc', label: 'Close modal · overlay · field' },
];

// isTypingTarget reports whether the event target is an editable field, where
// single-key shortcuts must stay dormant so the user can type freely.
export function isTypingTarget(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  const tag = el.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return el.isContentEditable === true;
}

// A minimal view of a KeyboardEvent — enough to resolve a shortcut, and trivial
// to construct in tests.
export interface KeyLike {
  key: string;
  ctrlKey?: boolean;
  metaKey?: boolean;
  altKey?: boolean;
  target?: EventTarget | null;
}

// resolveShortcut maps a keydown to a Shortcut, or null when it should be
// ignored (modifier combos, typing in a field, unbound keys). Escape is the one
// binding that fires even while typing — so it can bail out of a field or close
// a modal whose input has focus.
export function resolveShortcut(e: KeyLike): Shortcut | null {
  if (e.ctrlKey || e.metaKey || e.altKey) return null; // leave browser combos alone
  if (e.key === 'Escape') return { kind: 'close' };
  if (isTypingTarget(e.target ?? null)) return null;
  switch (e.key) {
    case '?': return { kind: 'help' };
    case 'n': return { kind: 'new' };
    case '/': return { kind: 'filter' };
    case 'r': return { kind: 'refresh' };
    case 'j': return { kind: 'nav', delta: 1 };
    case 'k': return { kind: 'nav', delta: -1 };
  }
  if (e.key.length === 1 && e.key >= '1' && e.key <= '9') {
    return { kind: 'tab', index: Number(e.key) };
  }
  return null;
}
