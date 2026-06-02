export type Status =
  | 'spawning' | 'working' | 'waiting_for_input'
  | 'idle' | 'done' | 'errored' | 'orphaned';

export interface AgentEvent {
  ts: string;
  type: string;
  detail: string;
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
}
