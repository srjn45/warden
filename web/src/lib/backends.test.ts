import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  listBackends, rescanBackends, setDefaultBackend, setThinkingMode, patchBackend,
  ApiError, type Backend,
} from './api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  });
}

const SETTINGS = { id: 'settings', internal_thinking_mode: 'local_only', allow_paid_autopilot: false };

function backend(over: Partial<Backend> = {}): Backend {
  return {
    id: 'codex', installed: true, binary_path: '/usr/bin/codex', detected_at: '2026-08-06T00:00:00Z',
    tier: 'free', default: false, enabled: true, is_local: false, ...over,
  };
}

beforeEach(() => { vi.restoreAllMocks(); });

describe('backends api', () => {
  it('listBackends GETs /backends and unwraps the state', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
      backends: [backend()], settings: SETTINGS,
    }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listBackends();
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/backends');
    expect(out.backends).toHaveLength(1);
    expect(out.backends[0].id).toBe('codex');
    expect(out.settings.internal_thinking_mode).toBe('local_only');
  });

  it('listBackends returns [] backends when the body array is null', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ backends: null, settings: SETTINGS })));
    const out = await listBackends();
    expect(out.backends).toEqual([]);
    expect(out.settings.id).toBe('settings');
  });

  it('rescanBackends POSTs to /backends/rescan', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ backends: [backend()], settings: SETTINGS }));
    vi.stubGlobal('fetch', fetchMock);
    await rescanBackends();
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/backends/rescan');
    expect(opts.method).toBe('POST');
  });

  it('setDefaultBackend PUTs the id to /backends/default', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
      backends: [backend({ default: true })], settings: SETTINGS,
    }));
    vi.stubGlobal('fetch', fetchMock);
    await setDefaultBackend('codex');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/backends/default');
    expect(opts.method).toBe('PUT');
    expect(JSON.parse(opts.body)).toEqual({ id: 'codex' });
  });

  it('setDefaultBackend surfaces a 400 (local/uninstalled) as ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      jsonResponse({ error: 'local cannot be the default backend' }, 400),
    ));
    await expect(setDefaultBackend('local')).rejects.toBeInstanceOf(ApiError);
  });

  it('setThinkingMode PUTs the mode to /backends/thinking-mode', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ ...SETTINGS, internal_thinking_mode: 'free_plus_local' }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await setThinkingMode('free_plus_local');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/backends/thinking-mode');
    expect(opts.method).toBe('PUT');
    expect(JSON.parse(opts.body)).toEqual({ mode: 'free_plus_local' });
    expect(out.internal_thinking_mode).toBe('free_plus_local');
  });

  it('patchBackend PATCHes the tier to /backends/{id}', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(backend({ tier: 'subscription' })));
    vi.stubGlobal('fetch', fetchMock);
    const out = await patchBackend('codex', { tier: 'subscription' });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/backends/codex');
    expect(opts.method).toBe('PATCH');
    expect(JSON.parse(opts.body)).toEqual({ tier: 'subscription' });
    expect(out.tier).toBe('subscription');
  });

  it('patchBackend PATCHes the enabled flag only', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(backend({ enabled: false })));
    vi.stubGlobal('fetch', fetchMock);
    await patchBackend('codex', { enabled: false });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ enabled: false });
  });

  it('patchBackend encodes the id in the path', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(backend()));
    vi.stubGlobal('fetch', fetchMock);
    await patchBackend('a/b', { enabled: true });
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/backends/a%2Fb');
  });

  it('patchBackend surfaces a 400 (re-tier local) as ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      jsonResponse({ error: 'the local tier is system-set' }, 400),
    ));
    await expect(patchBackend('local', { tier: 'free' })).rejects.toBeInstanceOf(ApiError);
  });
});
