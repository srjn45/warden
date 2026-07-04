import { describe, it, expect, vi, beforeEach } from 'vitest';
import { quickAdd } from './quickadd';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => { vi.restoreAllMocks(); });

describe('quickAdd', () => {
  it('spawns a no-prompt unsupervised agent in dir and returns the id', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-1' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    const out = await quickAdd('/work/project');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/spawn');
    expect(JSON.parse(opts.body)).toEqual({
      type: '', ticket: '', repo: '', branch: '', pr: '', worktree: false,
      prompt: '', cwd: '/work/project', supervised: false, role: '', force: false,
    });
    expect(out).toEqual({ kind: 'created', id: 'agent-1' });
  });

  it('passes force through on retry', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-2' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await quickAdd('/work/project', true);
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).force).toBe(true);
  });

  it('maps a 428 memory-pressure verdict to a confirm result', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ verdict: { reason: 'memory at 92%' } }, 428),
    );
    vi.stubGlobal('fetch', fetchMock);
    const out = await quickAdd('/work/project');
    expect(out).toEqual({ kind: 'confirm', reason: 'memory at 92%' });
  });

  it('maps any other failure to an error result', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ error: 'boom' }, 500),
    );
    vi.stubGlobal('fetch', fetchMock);
    const out = await quickAdd('/work/project');
    expect(out).toEqual({ kind: 'error', message: 'boom' });
  });

  it('maps a network failure (fetch rejects) to an error result', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')));
    const out = await quickAdd('/work/project');
    expect(out).toEqual({ kind: 'error', message: 'Failed to fetch' });
  });
});
