import type { Status } from './types';

export type BadgeKind = 'busy' | 'attention' | 'idle' | 'error';
export interface Badge { label: string; kind: BadgeKind; }

export function busyIdle(status: Status, exitCode?: number | null): Badge {
  switch (status) {
    case 'spawning': return { label: 'Starting', kind: 'busy' };
    case 'working': return { label: 'Busy', kind: 'busy' };
    case 'waiting_for_input': return { label: 'Needs input', kind: 'attention' };
    case 'idle': return { label: 'Idle', kind: 'idle' };
    case 'done': return { label: 'Done', kind: 'idle' };
    case 'errored':
      return { label: exitCode != null && exitCode !== 0 ? `Error (${exitCode})` : 'Error', kind: 'error' };
    case 'orphaned': return { label: 'Orphaned', kind: 'error' };
    default: return { label: status, kind: 'idle' };
  }
}
