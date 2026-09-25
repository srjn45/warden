import { afterEach, expect, it, vi } from 'vitest';
import { getTree, subscribeSessions } from './api';
import { setToken, clearToken } from './token';
afterEach(() => { clearToken(); vi.unstubAllGlobals(); });
it('fetches the authoritative tree through the authenticated API', async () => {
  const tree = { roots: [{ type: 'project', id: 'empty', children: [] }], degraded: true };
  const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(tree)));
  vi.stubGlobal('fetch', fetchMock); setToken('tree-token');
  expect(await getTree()).toEqual(tree);
  expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/tree');
  expect(new Headers(fetchMock.mock.calls[0][1].headers).get('Authorization')).toBe('Bearer tree-token');
});
it('receives named tree frames separately, ignores malformed frames, and closes the stream', () => {
  let source: FakeEventSource;
  class FakeEventSource {
    onopen?: () => void;
    onmessage?: (e: MessageEvent) => void;
    onerror?: () => void;
    listeners = new Map<string, (event: MessageEvent) => void>();
    close = vi.fn();
    constructor(_url: string) { source = this; }
    addEventListener(name: string, callback: (event: MessageEvent) => void) { this.listeners.set(name, callback); }
  }
  vi.stubGlobal('EventSource', FakeEventSource);
  const onData = vi.fn(), onTree = vi.fn();
  const unsubscribe = subscribeSessions(onData, vi.fn(), vi.fn(), undefined, onTree);
  const tree = { roots: [{ type: 'project', id: 'empty' }] };
  source!.listeners.get('tree')!(new MessageEvent('tree', { data: JSON.stringify(tree) }));
  expect(onTree).toHaveBeenCalledWith(tree); expect(onData).not.toHaveBeenCalled();
  source!.listeners.get('tree')!(new MessageEvent('tree', { data: '{' }));
  expect(onTree).toHaveBeenCalledTimes(1);
  unsubscribe(); expect(source!.close).toHaveBeenCalledOnce();
});
