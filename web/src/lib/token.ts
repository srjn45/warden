// Token storage for remote access. When the daemon binds a non-loopback
// address it requires a bearer token (see internal/auth). The browser keeps the
// user-supplied token in localStorage and attaches it to every request: as an
// Authorization header for fetch, and as a ?token= query param for transports
// that can't set headers (EventSource, WebSocket).

const TOKEN_KEY = 'warden_token';

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY)?.trim() ?? '';
  } catch {
    return ''; // storage unavailable (private mode, SSR)
  }
}

export function setToken(value: string): void {
  try {
    localStorage.setItem(TOKEN_KEY, value.trim());
  } catch { /* ignore */ }
}

export function clearToken(): void {
  try {
    localStorage.removeItem(TOKEN_KEY);
  } catch { /* ignore */ }
}

export function hasToken(): boolean {
  return getToken() !== '';
}

// withToken appends the stored token as a ?token= query param, for transports
// that cannot send an Authorization header. Returns the URL unchanged when no
// token is stored.
export function withToken(url: string): string {
  const t = getToken();
  if (!t) return url;
  const sep = url.includes('?') ? '&' : '?';
  return `${url}${sep}token=${encodeURIComponent(t)}`;
}

// Auth-required signal: apiFetch fires this when the daemon answers 401, so the
// UI (Dashboard) can surface the token-entry modal from any call site.
type AuthListener = () => void;
const listeners = new Set<AuthListener>();

export function onAuthRequired(fn: AuthListener): () => void {
  listeners.add(fn);
  return () => { listeners.delete(fn); };
}

export function notifyAuthRequired(): void {
  for (const fn of listeners) fn();
}
