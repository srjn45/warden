import type { Session, ApprovalView, Pipeline, Digest, ContextEntry, Message, Conflict } from './types';
import type { Verdict, PressureStatus } from './pressure';
import type { MetricsSample } from './metrics';
import type { Summary } from './savings';
import type { Report as SpendReport } from './spend';
import { getToken, withToken, notifyAuthRequired, API_PREFIX } from './token';

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
  const res = await fetch(API_PREFIX + input, { ...init, headers });
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
  role?: string;
  force?: boolean;
  // kind selects the session kind. Omitted/'' spawns an AI agent (the default);
  // 'terminal' spawns a plain shell session — the daemon then ignores
  // backend/model/role/prompt and forces free-form. See createTerminal.
  kind?: string;
}

// RoleInfo is a built-in agent role for the picker (GET /roles): name + a
// one-line description. The empty/"general" role injects no persona.
export interface RoleInfo { name: string; description: string; }

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
      role: p.role ?? '', force: !!p.force, kind: p.kind ?? '',
    }),
  });
  if (res.status === 428) {
    const body = await res.json() as { verdict: Verdict };
    throw new ConfirmationRequiredError(body.verdict);
  }
  return parse<Session>(res);
}

// createTerminal spawns a plain terminal session (kind=terminal) in `cwd`. The
// daemon ignores backend/model/role/prompt for terminals and runs ${SHELL:-bash}
// with inherited env; an empty cwd lets it fall back to its launch directory.
export async function createTerminal(cwd: string): Promise<Session> {
  return spawn({ kind: 'terminal', cwd });
}

// listRoles returns warden's built-in agent roles (GET /roles): general first,
// then alphabetical. Used to populate the new-agent role picker.
export async function listRoles(): Promise<RoleInfo[]> {
  const data = await parse<{ roles: RoleInfo[] | null }>(await apiFetch('/roles'));
  return data.roles ?? [];
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
// bucket='day'|'hour' to attach the zero-filled saved-tokens trend the Metrics
// tab plots at that granularity, and an optional sinceISO window (RFC3339 or a
// duration like "7d"). The route is GATED: when savings is disabled the daemon
// answers 403, surfaced here as an ApiError(403) the caller turns into a
// friendly "enable savings" hint rather than an empty chart.
export async function getSavings(sinceISO?: string, bucket?: 'day' | 'hour'): Promise<Summary> {
  const q = new URLSearchParams();
  if (sinceISO) q.set('since', sinceISO);
  if (bucket) q.set('bucket', bucket);
  const qs = q.toString();
  return parse<Summary>(await apiFetch(`/savings${qs ? `?${qs}` : ''}`));
}

// getSpend returns the cost rollup (GET /spend): measured Claude spend priced
// per model and aggregated per-agent / per-repo / per-day, plus the daily/weekly
// totals the budget gate enforces. GATED by the same `savings` setting as the
// ledger: a disabled daemon answers 403, surfaced here as an ApiError(403) the
// Metrics tab turns into an enable hint.
export async function getSpend(): Promise<SpendReport> {
  return parse<SpendReport>(await apiFetch('/spend'));
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

// AutopilotBrain describes the run's brain agent (null in the S1 inert core).
export interface AutopilotBrain {
  agent_id: string;
  backend: string;
  tier: string;
  last_heartbeat: string;
  context_level: string;
}

// AutopilotTaskCounts is the ledger task rollup shown in status.
export interface AutopilotTaskCounts {
  pending: number;
  in_progress: number;
  landed: number;
}

// AutopilotBackoff describes the guardian's capped backoff (null unless degraded).
export interface AutopilotBackoff {
  stage: number;
  next_retry_at: string;
  last_error: string;
}

// AutopilotRun is one run's slice of the overall status.
export interface AutopilotRun {
  run_id: string;
  plan_file: string;
  repo: string;
  state: string;
  gate: string;
  brain: AutopilotBrain | null;
  workers_in_flight: number;
  tasks: AutopilotTaskCounts;
  backoff: AutopilotBackoff | null;
  landed_total: number;
}

// AutopilotStatus is the full response shape for GET/POST /autopilot.
export interface AutopilotStatus {
  enabled: boolean;
  runs: AutopilotRun[];
}

// AutopilotPreflightError represents a 409 from POST /autopilot when the
// enable-time preflight fails (autopilot.md §5.1).
export class AutopilotPreflightError extends Error {
  constructor(public failures: string[]) {
    super(`autopilot preflight failed (${failures.length} issue${failures.length === 1 ? '' : 's'})`);
    this.name = 'AutopilotPreflightError';
  }
}

// getAutopilot fetches the current autopilot status (GET /autopilot).
export async function getAutopilot(): Promise<AutopilotStatus> {
  const data = await parse<AutopilotStatus>(await apiFetch('/autopilot'));
  return { ...data, runs: data.runs ?? [] };
}

// setAutopilot flips the master switch (POST /autopilot). A 409 from the
// daemon's enable-time preflight surfaces as AutopilotPreflightError so the UI
// can render the full failure list with an init hint.
export async function setAutopilot(enabled: boolean): Promise<AutopilotStatus> {
  const res = await apiFetch('/autopilot', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  });
  if (res.status === 409) {
    let failures: string[] = [];
    try {
      const body = await res.json() as { failures?: string[] };
      failures = body.failures ?? [];
    } catch { /* non-JSON body */ }
    throw new AutopilotPreflightError(failures);
  }
  return parse<AutopilotStatus>(res);
}

// --- Backend registry (docs/specs/2026-08-06-backend-registry.md) -----------
//
// The agent-backend registry is warden's source of truth for which backend CLIs
// exist, their billing tier, which one is the default, and whether each is
// enabled — plus a store-level settings singleton (internal-thinking routing
// mode). These mirror the daemon's Backend / BackendSettings / BackendsState
// schemas exactly.

// Backend is one row of the registry. Detection fields (installed / binary_path
// / detected_at) are facts a rescan refreshes; tier / default / enabled are user
// preferences a rescan preserves. is_local marks the reserved $0 local-model row
// (never limited, never a user default). limited_until is the RFC3339 instant a
// rate-limit lifts (absent/zero when the backend is available).
export interface Backend {
  id: string;
  installed: boolean;
  binary_path: string;
  detected_at: string;
  tier: string; // free | subscription | pay_per_use | unclassified | local
  default: boolean;
  enabled: boolean;
  is_local: boolean;
  limited_until?: string;
}

// BackendSettings is the store-level policy singleton.
export interface BackendSettings {
  id: string;
  internal_thinking_mode: string; // local_only | free_plus_local
  allow_paid_autopilot: boolean;
}

// BackendsState is the full registry (rows sorted by id) plus settings.
export interface BackendsState {
  backends: Backend[];
  settings: BackendSettings;
}

function unwrapBackends(data: { backends: Backend[] | null; settings: BackendSettings }): BackendsState {
  return { backends: data.backends ?? [], settings: data.settings };
}

// listBackends returns the persisted registry + settings (GET /backends).
export async function listBackends(): Promise<BackendsState> {
  return unwrapBackends(await parse(await apiFetch('/backends')));
}

// rescanBackends re-detects installed CLIs and returns the refreshed registry
// (POST /backends/rescan). tier/default/enabled preferences are preserved.
export async function rescanBackends(): Promise<BackendsState> {
  return unwrapBackends(await parse(await apiFetch('/backends/rescan', { method: 'POST' })));
}

// setDefaultBackend marks one backend the single default (PUT /backends/default).
// The daemon rejects an unknown/uninstalled/disabled backend and the reserved
// local row with a 4xx, surfaced as an ApiError.
export async function setDefaultBackend(id: string): Promise<BackendsState> {
  return unwrapBackends(await parse(await apiFetch('/backends/default', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id }),
  })));
}

// setThinkingMode sets the internal-thinking routing mode (PUT
// /backends/thinking-mode): 'local_only' keeps warden's own thinking on the $0
// local model; 'free_plus_local' prefers free cloud backends. Returns the
// updated settings.
export async function setThinkingMode(mode: string): Promise<BackendSettings> {
  return parse<BackendSettings>(await apiFetch('/backends/thinking-mode', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode }),
  }));
}

// patchBackend updates one backend's tier and/or enabled flag (PATCH
// /backends/{id}); an omitted field is left unchanged. The daemon rejects a
// re-tier of the reserved local row with a 4xx. Returns the updated row.
export async function patchBackend(
  id: string,
  patch: { tier?: string; enabled?: boolean },
): Promise<Backend> {
  return parse<Backend>(await apiFetch(`/backends/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(patch),
  }));
}

// subscribeSessions opens an SSE connection. Returns an unsubscribe function.
export function subscribeSessions(
  onData: (sessions: Session[]) => void,
  onError: () => void,
  onOpen: () => void,
): () => void {
  const es = new EventSource(withToken(API_PREFIX + '/events/stream'));
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
