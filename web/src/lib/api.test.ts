import { describe, it, expect, vi, beforeEach } from 'vitest';
import { listSessions, spawn, listDirs, terminate, removeWorktree, deleteSession, ApiError } from './api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => { vi.restoreAllMocks(); });

describe('api', () => {
  it('listSessions GETs /sessions and unwraps the array', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ sessions: [{ id: 'A-1' }] }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listSessions();
    expect(fetchMock).toHaveBeenCalledWith('/sessions');
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe('A-1');
  });

  it('spawn POSTs the full body to /spawn', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'A-1' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ type: 'development', repo: '/r', ticket: 'A-1' });
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/spawn');
    expect(opts.method).toBe('POST');
    expect(JSON.parse(opts.body)).toEqual({
      type: 'development', ticket: 'A-1', repo: '/r', branch: '', pr: '', worktree: false, prompt: '', cwd: '',
    });
  });

  it('spawn supports a prompt-only body', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-x' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ prompt: 'do research on X' });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      type: '', ticket: '', repo: '', branch: '', pr: '', worktree: false, prompt: 'do research on X', cwd: '',
    });
  });

  it('terminate POSTs to /sessions/{id}/terminate', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'terminated' }));
    vi.stubGlobal('fetch', fetchMock);
    await terminate('A-1');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/sessions/A-1/terminate');
    expect(opts.method).toBe('POST');
  });

  it('removeWorktree POSTs force to /sessions/{id}/remove-worktree', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'worktree removed' }));
    vi.stubGlobal('fetch', fetchMock);
    await removeWorktree('A-1', true);
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/sessions/A-1/remove-worktree');
    expect(JSON.parse(opts.body)).toEqual({ force: true });
  });

  it('deleteSession POSTs hard to /sessions/{id}/delete', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'deleted' }));
    vi.stubGlobal('fetch', fetchMock);
    await deleteSession('A-1', true);
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/sessions/A-1/delete');
    expect(JSON.parse(opts.body)).toEqual({ hard: true });
  });

  it('spawn includes cwd when provided', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-x' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ prompt: 'do X', cwd: '/work/project' });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).cwd).toBe('/work/project');
  });

  it('listDirs GETs /fs/dirs with the path query', async () => {
    const listing = { path: '/work', parent: '/', entries: [{ name: 'project', path: '/work/project' }] };
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(listing));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listDirs('/work');
    expect(fetchMock).toHaveBeenCalledWith('/fs/dirs?path=%2Fwork');
    expect(out.entries[0].path).toBe('/work/project');
  });

  it('throws ApiError with the server error message on non-2xx', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ error: 'already exists' }, 409));
    vi.stubGlobal('fetch', fetchMock);
    await expect(spawn({ type: 'development', repo: '/r' })).rejects.toMatchObject({
      status: 409, message: 'already exists',
    });
    await expect(spawn({ type: 'development', repo: '/r' })).rejects.toBeInstanceOf(ApiError);
  });
});
