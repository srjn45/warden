import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { listSessions, spawn, listDirs, terminate, removeWorktree, deleteSession, ApiError, listApprovals, approve, listPipelines, cancelPipeline, deletePipeline, retryJob, createPipeline, startPipeline, listConflicts } from './api';
import { setToken, clearToken, onAuthRequired } from './token';

function authHeader(call: unknown[]): string | null {
  const init = call[1] as RequestInit | undefined;
  return new Headers(init?.headers).get('Authorization');
}

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
    expect(fetchMock.mock.calls[0][0]).toBe('/sessions');
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
      type: 'development', ticket: 'A-1', repo: '/r', branch: '', pr: '', worktree: false, prompt: '', cwd: '', supervised: false, force: false,
    });
  });

  it('spawn supports a prompt-only body', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-x' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ prompt: 'do research on X' });
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({
      type: '', ticket: '', repo: '', branch: '', pr: '', worktree: false, prompt: 'do research on X', cwd: '', supervised: false, force: false,
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
    expect(fetchMock.mock.calls[0][0]).toBe('/fs/dirs?path=%2Fwork');
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

  it('listApprovals returns the queue', async () => {
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ enabled: true, approvals: [{ id: 'a1', recognized: true, options: ['Yes', 'No'], fingerprint: 'ff' }] }),
    }) as any;
    const r = await listApprovals();
    expect(r.enabled).toBe(true);
    expect(r.approvals[0].id).toBe('a1');
  });

  it('approve posts option + fingerprint', async () => {
    const f = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ status: 'answered' }) });
    globalThis.fetch = f as any;
    await approve('a1', 2, 'ff');
    expect(f).toHaveBeenCalledWith('/sessions/a1/approve', expect.objectContaining({ method: 'POST' }));
    const body = JSON.parse((f.mock.calls[0][1] as any).body);
    expect(body).toEqual({ option: 2, fingerprint: 'ff' });
  });

  it('spawn sends supervised', async () => {
    const f = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ id: 'a1' }) });
    globalThis.fetch = f as any;
    await spawn({ prompt: 'x', supervised: true });
    const body = JSON.parse((f.mock.calls[0][1] as any).body);
    expect(body.supervised).toBe(true);
  });
});

describe('auth', () => {
  afterEach(() => clearToken());

  it('attaches a Bearer header when a token is stored', async () => {
    setToken('s3cret');
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ sessions: [] }));
    vi.stubGlobal('fetch', fetchMock);
    await listSessions();
    expect(authHeader(fetchMock.mock.calls[0])).toBe('Bearer s3cret');
  });

  it('sends no Authorization header when no token is stored', async () => {
    clearToken();
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ sessions: [] }));
    vi.stubGlobal('fetch', fetchMock);
    await listSessions();
    expect(authHeader(fetchMock.mock.calls[0])).toBeNull();
  });

  it('preserves the per-request Content-Type while adding auth', async () => {
    setToken('tok');
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'A-1' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await spawn({ prompt: 'x' });
    const h = new Headers((fetchMock.mock.calls[0][1] as RequestInit).headers);
    expect(h.get('Authorization')).toBe('Bearer tok');
    expect(h.get('Content-Type')).toBe('application/json');
  });

  it('fires the auth-required signal on a 401 and still throws ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: 'unauthorized' }, 401)));
    let fired = false;
    const off = onAuthRequired(() => { fired = true; });
    await expect(listSessions()).rejects.toBeInstanceOf(ApiError);
    expect(fired).toBe(true);
    off();
  });
});

describe('collab api', () => {
  it('listConflicts GETs /collab/conflicts and unwraps the array', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({
      conflicts: [{ file: 'internal/auth.go', agents: [{ id: 'A-1', name: 'alpha' }, { id: 'B-2' }] }],
    }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listConflicts();
    expect(fetchMock.mock.calls[0][0]).toBe('/collab/conflicts');
    expect(out).toHaveLength(1);
    expect(out[0].file).toBe('internal/auth.go');
    expect(out[0].agents).toHaveLength(2);
  });

  it('listConflicts returns [] when the body is null', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ conflicts: null })));
    expect(await listConflicts()).toEqual([]);
  });
});

describe('pipelines api', () => {
  it('listPipelines GETs /pipelines and unwraps the array', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ pipelines: [{ id: 'demo', jobs: [] }] }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listPipelines();
    expect(fetchMock.mock.calls[0][0]).toBe('/pipelines');
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe('demo');
  });

  it('listPipelines returns [] when the body is null', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ pipelines: null })));
    expect(await listPipelines()).toEqual([]);
  });

  it('cancelPipeline POSTs to the cancel endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'canceled' }));
    vi.stubGlobal('fetch', fetchMock);
    await cancelPipeline('demo');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/pipelines/demo/cancel');
    expect(opts.method).toBe('POST');
  });

  it('deletePipeline DELETEs the pipeline endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'deleted' }));
    vi.stubGlobal('fetch', fetchMock);
    await deletePipeline('demo');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/pipelines/demo');
    expect(opts.method).toBe('DELETE');
  });

  it('deletePipeline surfaces a 409 (live jobs) as ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      jsonResponse({ error: 'pipeline has live jobs — cancel it first' }, 409),
    ));
    await expect(deletePipeline('demo')).rejects.toBeInstanceOf(ApiError);
  });

  it('retryJob POSTs to the job retry endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'retrying' }));
    vi.stubGlobal('fetch', fetchMock);
    await retryJob('demo', 'a');
    expect(fetchMock.mock.calls[0][0]).toBe('/pipelines/demo/jobs/a/retry');
    expect(fetchMock.mock.calls[0][1].method).toBe('POST');
  });

  it('createPipeline POSTs the spec to /pipelines', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'demo', jobs: [] }, 201));
    vi.stubGlobal('fetch', fetchMock);
    const out = await createPipeline('{"name":"demo"}');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/pipelines');
    expect(opts.method).toBe('POST');
    expect(JSON.parse(opts.body)).toEqual({ spec: '{"name":"demo"}' });
    expect(out.id).toBe('demo');
  });

  it('createPipeline surfaces a 400 (bad spec) as ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: 'pipeline repo is required' }, 400)));
    await expect(createPipeline('{}')).rejects.toBeInstanceOf(ApiError);
  });

  it('startPipeline POSTs to the start endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'started' }));
    vi.stubGlobal('fetch', fetchMock);
    await startPipeline('demo');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/pipelines/demo/start');
    expect(opts.method).toBe('POST');
  });
});
