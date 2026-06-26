import type { Session, ApprovalView, Pipeline, Digest, ContextEntry, Message, Conflict } from './types';
import type { Verdict, PressureStatus } from './pressure';
import type { MetricsSample } from './metrics';
import type { Summary } from './savings';
import { getToken, withToken, notifyAuthRequired } from './token';

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = 'ApiError';
  }
}

// apiFetch is the single seam every REST call goes through. It attaches the
// stored bearer token (when set) and, on a 401, signals the UI to prompt for a
// token before returning the response to the caller's normal error handling.
async function apiFetch(input: string, init?: RequestInit): Promise<Response> {
  const token = getToken();
  const headers = new Headers(init?.headers);
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const res = await fetch(input, { ...init, headers });
  if (res.status === 401) notifyAuthRequired();
  return res;
}

export class ConfirmationRequiredError extends Error {
  constructor(public verdict: Verdict) {
    super(verdict.reason);
    this.name = 'ConfirmationRequiredError';
  }
}

export interface SpawnParams {
  type?: string;
  repo?: string;
  ticket?: string;
  branch?: string;
  pr?: string;
  worktree?: boolean;
  prompt?: string;
  cwd?: string;
  supervised?: boolean;
  force?: boolean;
}

async function parse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body && body.error) msg = body.error;
    } catch { /* non-JSON error body */ }
    throw new ApiError(res.status, msg);
  }
  return res.json() as Promise<T>;
}

export async function listSessions(): Promise<Session[]> {
  const data = await parse<{ sessions: Session[] | null }>(await apiFetch('/sessions'));
  return data.sessions ?? [];
}

export async function getSession(id: string): Promise<Session> {
  return parse<Session>(await apiFetch(`/sessions/${encodeURIComponent(id)}`));
}

export async function spawn(p: SpawnParams): Promise<Session> {
  const res = await apiFetch('/spawn', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      type: p.type ?? '', ticket: p.ticket ?? '', repo: p.repo ?? '',
      branch: p.branch ?? '', pr: p.pr ?? '', worktree: !!p.worktree,
      prompt: p.prompt ?? '', cwd: p.cwd ?? '', supervised: !!p.supervised,
      force: !!p.force,
    }),
  });
  if (res.status === 428) {
    const body = await res.json() as { verdict: Verdict };
    throw new ConfirmationRequiredError(body.verdict);
  }
  return parse<Session>(res);
}

export interface DirEntry { name: string; path: string; }
export interface DirListing { path: string; parent: string; entries: DirEntry[]; }

export async function listDirs(path?: string): Promise<DirListing> {
  const q = path ? `?path=${encodeURIComponent(path)}` : '';
  return parse<DirListing>(await apiFetch(`/fs/dirs${q}`));
}

export async function terminate(id: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/sessions/${encodeURIComponent(id)}/terminate`, {
    method: 'POST',
  }));
}

export async function removeWorktree(id: string, force: boolean): Promise<void> {
  await parse<unknown>(await apiFetch(`/sessions/${encodeURIComponent(id)}/remove-worktree`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ force }),
  }));
}

export async function deleteSession(id: string, hard: boolean): Promise<void> {
  await parse<unknown>(await apiFetch(`/sessions/${encodeURIComponent(id)}/delete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ hard }),
  }));
}

// searchSessions runs the daemon's in-memory full-text search (GET /search).
// closed=true also searches archived agents. The daemon rejects a blank query
// with a 400, surfaced as an ApiError.
export async function searchSessions(q: string, closed = false): Promise<Session[]> {
  const params = new URLSearchParams({ q });
  if (closed) params.set('closed', 'true');
  const data = await parse<{ sessions: Session[] | null }>(
    await apiFetch(`/search?${params.toString()}`),
  );
  return data.sessions ?? [];
}

// getHistory browses the archived (closed/) store (GET /history), newest-first,
// narrowed by the optional sinceISO/type/limit filters.
export async function getHistory(
  opts: { sinceISO?: string; type?: string; limit?: number } = {},
): Promise<Session[]> {
  const q = new URLSearchParams();
  if (opts.sinceISO) q.set('since', opts.sinceISO);
  if (opts.type) q.set('type', opts.type);
  if (opts.limit) q.set('limit', String(opts.limit));
  const qs = q.toString();
  const data = await parse<{ sessions: Session[] | null }>(
    await apiFetch(`/history${qs ? `?${qs}` : ''}`),
  );
  return data.sessions ?? [];
}

// sendMessage delivers a directed message to an agent's inbox (POST
// /sessions/{id}/messages). Used by the bulk message action.
export async function sendMessage(id: string, body: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/sessions/${encodeURIComponent(id)}/messages`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ body }),
  }));
}

export async function getOutput(id: string, lines = 200): Promise<string> {
  const data = await parse<{ output: string }>(
    await apiFetch(`/sessions/${encodeURIComponent(id)}/output?lines=${lines}`),
  );
  return data.output;
}

export async function getDigest(id: string): Promise<Digest> {
  return parse<Digest>(await apiFetch(`/sessions/${encodeURIComponent(id)}/digest`));
}

export async function getPressure(): Promise<PressureStatus> {
  return parse<PressureStatus>(await apiFetch('/pressure'));
}

export async function getMetrics(): Promise<MetricsSample> {
  return parse<MetricsSample>(await apiFetch('/metrics'));
}

export async function getMetricsHistory(sinceISO?: string, limit = 480): Promise<MetricsSample[]> {
  const q = new URLSearchParams();
  if (sinceISO) q.set('since', sinceISO);
  if (limit) q.set('limit', String(limit));
  const qs = q.toString();
  const data = await parse<{ samples: MetricsSample[] | null }>(
    await apiFetch(`/metrics/history${qs ? `?${qs}` : ''}`),
  );
  return data.samples ?? [];
}

// getSavings returns the aggregated token-savings summary (GET /savings). Pass
// bucket='day' to attach the per-day saved-tokens trend the Metrics tab plots,
// and an optional sinceISO window (RFC3339 or a duration like "7d"). The route
// is GATED: when savings is disabled the daemon answers 403, surfaced here as
// an ApiError(403) the caller turns into a friendly "enable savings" hint
// rather than an empty chart.
export async function getSavings(sinceISO?: string, bucket?: 'day'): Promise<Summary> {
  const q = new URLSearchParams();
  if (sinceISO) q.set('since', sinceISO);
  if (bucket) q.set('bucket', bucket);
  const qs = q.toString();
  return parse<Summary>(await apiFetch(`/savings${qs ? `?${qs}` : ''}`));
}

export async function listApprovals(): Promise<{ enabled: boolean; approvals: ApprovalView[] }> {
  const data = await parse<{ enabled: boolean; approvals: ApprovalView[] | null }>(await apiFetch('/approvals'));
  return { enabled: data.enabled, approvals: data.approvals ?? [] };
}

export async function approve(id: string, option: number, fingerprint: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/sessions/${encodeURIComponent(id)}/approve`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ option, fingerprint }),
  }));
}

// listContext returns the daemon's shared-context entries (read-only inspector).
export async function listContext(): Promise<ContextEntry[]> {
  const data = await parse<{ entries: ContextEntry[] | null }>(await apiFetch('/context'));
  return data.entries ?? [];
}

// listMessages returns recent message traffic across ALL agent inboxes,
// newest-first. Read-only: unlike a per-agent inbox fetch, this never marks
// anything read.
export async function listMessages(limit = 100): Promise<Message[]> {
  const data = await parse<{ messages: Message[] | null }>(
    await apiFetch(`/messages?limit=${limit}`),
  );
  return data.messages ?? [];
}

// listConflicts returns files edited by two or more active agents right now.
// Read-only; the daemon recomputes on each request and always returns an array.
export async function listConflicts(): Promise<Conflict[]> {
  const data = await parse<{ conflicts: Conflict[] | null }>(await apiFetch('/collab/conflicts'));
  return data.conflicts ?? [];
}

export async function listPipelines(): Promise<Pipeline[]> {
  const data = await parse<{ pipelines: Pipeline[] | null }>(await apiFetch('/pipelines'));
  return data.pipelines ?? [];
}

// createPipeline submits a spec (raw YAML, or JSON which is a valid YAML subset)
// to the existing POST /pipelines. The daemon re-parses and re-validates it, so
// a 400 surfaces the authoritative validation error as an ApiError.
export async function createPipeline(spec: string): Promise<Pipeline> {
  return parse<Pipeline>(await apiFetch('/pipelines', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ spec }),
  }));
}

// startPipeline kicks off DAG reconciliation. The daemon refuses with 409 if the
// pipeline already started — surfaced as an ApiError.
export async function startPipeline(id: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/pipelines/${encodeURIComponent(id)}/start`, { method: 'POST' }));
}

// pausePipeline halts DAG progress: in-flight jobs keep running but no new job
// spawns until resumed. The daemon refuses with 409 if the pipeline is not
// running — surfaced as an ApiError.
export async function pausePipeline(id: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/pipelines/${encodeURIComponent(id)}/pause`, { method: 'POST' }));
}

// resumePipeline lifts a pause and reconciles. The daemon refuses with 409 if
// the pipeline is not paused — surfaced as an ApiError.
export async function resumePipeline(id: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/pipelines/${encodeURIComponent(id)}/resume`, { method: 'POST' }));
}

export async function cancelPipeline(id: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/pipelines/${encodeURIComponent(id)}/cancel`, { method: 'POST' }));
}

// deletePipeline removes a pipeline's record. The daemon refuses with 409 while
// any job is still live (running / needs_attention) — surfaced as an ApiError.
export async function deletePipeline(id: string): Promise<void> {
  await parse<unknown>(await apiFetch(`/pipelines/${encodeURIComponent(id)}`, { method: 'DELETE' }));
}

export async function retryJob(pid: string, job: string): Promise<void> {
  await parse<unknown>(await apiFetch(
    `/pipelines/${encodeURIComponent(pid)}/jobs/${encodeURIComponent(job)}/retry`,
    { method: 'POST' },
  ));
}

// subscribeSessions opens an SSE connection. Returns an unsubscribe function.
export function subscribeSessions(
  onData: (sessions: Session[]) => void,
  onError: () => void,
  onOpen: () => void,
): () => void {
  const es = new EventSource(withToken('/events/stream'));
  es.onopen = () => onOpen();
  es.onmessage = (e) => {
    try {
      const d = JSON.parse(e.data) as { sessions: Session[] | null };
      onData(d.sessions ?? []);
    } catch { /* ignore malformed frame */ }
  };
  es.onerror = () => onError();
  return () => es.close();
}
