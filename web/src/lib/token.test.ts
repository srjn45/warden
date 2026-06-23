import { describe, it, expect, beforeEach } from 'vitest';
import { getToken, setToken, clearToken, hasToken, withToken, onAuthRequired, notifyAuthRequired } from './token';

beforeEach(() => clearToken());

describe('token storage', () => {
  it('round-trips a token and trims whitespace', () => {
    setToken('  abc123  ');
    expect(getToken()).toBe('abc123');
    expect(hasToken()).toBe(true);
  });

  it('reports empty before any token is set', () => {
    expect(getToken()).toBe('');
    expect(hasToken()).toBe(false);
  });

  it('clears the token', () => {
    setToken('abc');
    clearToken();
    expect(getToken()).toBe('');
    expect(hasToken()).toBe(false);
  });
});

describe('withToken', () => {
  it('returns the url unchanged when no token is set', () => {
    expect(withToken('/events/stream')).toBe('/events/stream');
  });

  it('appends ?token= when the url has no query string', () => {
    setToken('tok');
    expect(withToken('/events/stream')).toBe('/events/stream?token=tok');
  });

  it('appends &token= when the url already has a query string', () => {
    setToken('tok');
    expect(withToken('/messages?limit=10')).toBe('/messages?limit=10&token=tok');
  });

  it('url-encodes the token', () => {
    setToken('a b/c');
    expect(withToken('ws://h/attach')).toBe('ws://h/attach?token=a%20b%2Fc');
  });
});

describe('auth-required signal', () => {
  it('notifies registered listeners and unsubscribes cleanly', () => {
    let count = 0;
    const off = onAuthRequired(() => { count += 1; });
    notifyAuthRequired();
    expect(count).toBe(1);
    off();
    notifyAuthRequired();
    expect(count).toBe(1);
  });
});
