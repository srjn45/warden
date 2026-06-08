import type { Session, ApprovalView, Pipeline, Digest, ContextEntry, Message } from './types';
import type { Verdict, PressureStatus } from './pressure';

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
    this.name = 'ApiError';
  }
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
  const data = await parse<{ sessions: Session[] | null }>(await fetch('/sessions'));
  return data.sessions ?? [];
}

export async function getSession(id: string): Promise<Session> {
  return parse<Session>(await fetch(`/sessions/${encodeURIComponent(id)}`));
}

export async function spawn(p: SpawnParams): Promise<Session> {
  const res = await fetch('/spawn', {
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
  return parse<DirListing>(await fetch(`/fs/dirs${q}`));
}

export async function terminate(id: string): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/terminate`, {
    method: 'POST',
  }));
}

export async function removeWorktree(id: string, force: boolean): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/remove-worktree`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ force }),
  }));
}

export async function deleteSession(id: string, hard: boolean): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/delete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ hard }),
  }));
}

export async function getOutput(id: string, lines = 200): Promise<string> {
  const data = await parse<{ output: string }>(
    await fetch(`/sessions/${encodeURIComponent(id)}/output?lines=${lines}`),
  );
  return data.output;
}

export async function getDigest(id: string): Promise<Digest> {
  return parse<Digest>(await fetch(`/sessions/${encodeURIComponent(id)}/digest`));
}

export async function getPressure(): Promise<PressureStatus> {
  return parse<PressureStatus>(await fetch('/pressure'));
}

export async function listApprovals(): Promise<{ enabled: boolean; approvals: ApprovalView[] }> {
  const data = await parse<{ enabled: boolean; approvals: ApprovalView[] | null }>(await fetch('/approvals'));
  return { enabled: data.enabled, approvals: data.approvals ?? [] };
}

export async function approve(id: string, option: number, fingerprint: string): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/approve`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ option, fingerprint }),
  }));
}

// listContext returns the daemon's shared-context entries (read-only inspector).
export async function listContext(): Promise<ContextEntry[]> {
  const data = await parse<{ entries: ContextEntry[] | null }>(await fetch('/context'));
  return data.entries ?? [];
}

// listMessages returns recent message traffic across ALL agent inboxes,
// newest-first. Read-only: unlike a per-agent inbox fetch, this never marks
// anything read.
export async function listMessages(limit = 100): Promise<Message[]> {
  const data = await parse<{ messages: Message[] | null }>(
    await fetch(`/messages?limit=${limit}`),
  );
  return data.messages ?? [];
}

export async function listPipelines(): Promise<Pipeline[]> {
  const data = await parse<{ pipelines: Pipeline[] | null }>(await fetch('/pipelines'));
  return data.pipelines ?? [];
}

// createPipeline submits a spec (raw YAML, or JSON which is a valid YAML subset)
// to the existing POST /pipelines. The daemon re-parses and re-validates it, so
// a 400 surfaces the authoritative validation error as an ApiError.
export async function createPipeline(spec: string): Promise<Pipeline> {
  return parse<Pipeline>(await fetch('/pipelines', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ spec }),
  }));
}

// startPipeline kicks off DAG reconciliation. The daemon refuses with 409 if the
// pipeline already started — surfaced as an ApiError.
export async function startPipeline(id: string): Promise<void> {
  await parse<unknown>(await fetch(`/pipelines/${encodeURIComponent(id)}/start`, { method: 'POST' }));
}

export async function cancelPipeline(id: string): Promise<void> {
  await parse<unknown>(await fetch(`/pipelines/${encodeURIComponent(id)}/cancel`, { method: 'POST' }));
}

// deletePipeline removes a pipeline's record. The daemon refuses with 409 while
// any job is still live (running / needs_attention) — surfaced as an ApiError.
export async function deletePipeline(id: string): Promise<void> {
  await parse<unknown>(await fetch(`/pipelines/${encodeURIComponent(id)}`, { method: 'DELETE' }));
}

export async function retryJob(pid: string, job: string): Promise<void> {
  await parse<unknown>(await fetch(
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
  const es = new EventSource('/events/stream');
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
