import type { Status } from './types';

export type AgentState = 'pending' | 'busy' | 'idle' | 'need-input' | 'done' | 'orphaned' | 'rate_limited';
export type BadgeKind = 'busy' | 'attention' | 'idle' | 'error';
export interface Badge { label: AgentState; kind: BadgeKind; }

// Presentation only: API and persisted statuses retain their established values.
export function presentedStatus(status: Status, exitCode?: number | null): AgentState {
  switch (status) {
    case 'spawning': return 'pending';
    case 'working': return 'busy';
    case 'waiting_for_input': return 'need-input';
    case 'idle': case 'done': case 'orphaned': case 'rate_limited': return status;
    case 'errored': return exitCode != null ? 'done' : 'orphaned';
    default: return 'pending';
  }
}

export function busyIdle(status: Status, exitCode?: number | null): Badge {
  const label = presentedStatus(status, exitCode);
  const kind = label === 'busy' || label === 'pending' ? 'busy'
    : label === 'need-input' || label === 'rate_limited' ? 'attention'
    : label === 'orphaned' ? 'error' : 'idle';
  return { label, kind };
}
