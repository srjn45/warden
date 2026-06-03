import type { Session } from './types';

export interface FleetStats {
  total: number;
  busy: number;    // working | spawning
  waiting: number; // waiting_for_input
  errored: number; // errored | orphaned
}

export function deriveFleetStats(sessions: Session[]): FleetStats {
  const stats: FleetStats = { total: sessions.length, busy: 0, waiting: 0, errored: 0 };
  for (const s of sessions) {
    if (s.status === 'working' || s.status === 'spawning') stats.busy++;
    else if (s.status === 'waiting_for_input') stats.waiting++;
    else if (s.status === 'errored' || s.status === 'orphaned') stats.errored++;
  }
  return stats;
}
