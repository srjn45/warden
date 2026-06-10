export type Status =
  | 'spawning' | 'working' | 'waiting_for_input'
  | 'idle' | 'done' | 'errored' | 'orphaned';

export interface AgentEvent {
  ts: string;
  type: string;
  detail: string;
}

export interface ApprovalView {
  id: string;
  action: string;
  question: string;
  options: string[];
  fingerprint: string;
  recognized: boolean;
}

export interface FileChange {
  path: string;
  added: number;
  removed: number;
  edited: boolean;
}

export interface Digest {
  summary: string;
  files: FileChange[] | null;
  branch: string;
  turns: number;
  status: string;
  task: string;
}

export interface Session {
  id: string;
  type: string;
  ticket: string;
  tmux_session: string;
  repo: string;
  worktree: string;
  branch: string;
  pr: string;
  prompt: string;
  workdir: string;
  subject: string;
  status: Status;
  pid: number;
  created_at: string;
  updated_at: string;
  events: AgentEvent[] | null;
  last_pane_excerpt: string;
  supervised: boolean;
  exit_code?: number | null;
  context_tokens?: number;
  context_state?: '' | 'ok' | 'warning' | 'critical';
  context_checked_at?: string;
  last_compact_at?: string | null;
}

// ContextEntry mirrors the daemon's shared-context entry (GET /context).
export interface ContextEntry {
  key: string;
  value: string;
  updated_by: string;
  updated_at: string;
}

// Message mirrors the daemon's mailbox message (GET /messages — read-only).
export interface Message {
  id: string;
  from: string;
  to: string;
  body: string;
  ts: string;
  read: boolean;
}

export type PipelineStatus = 'pending' | 'running' | 'done' | 'stalled' | 'canceled';
export type PipelineJobStatus =
  | 'pending' | 'running' | 'done' | 'failed' | 'skipped' | 'needs_attention';

export interface PipelineJob {
  id: string;
  prompt: string;
  depends_on: string[] | null;
  handoff: string;
  worktree: string;
  supervised: boolean;
  type: string;
  session_id: string;
  status: PipelineJobStatus;
  output: string;
  branch?: string;
  digest?: Digest;
}

export interface Pipeline {
  id: string;
  name: string;
  repo: string;
  status: PipelineStatus;
  jobs: PipelineJob[];
}
