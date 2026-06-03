import type { Session } from './types';

// needsAttention selects agents that are blocked on the user or have failed:
// waiting_for_input (blocked), errored / orphaned (failed). Input order is kept.
export function needsAttention(sessions: Session[]): Session[] {
  return sessions.filter(
    (s) => s.status === 'waiting_for_input' || s.status === 'errored' || s.status === 'orphaned',
  );
}
