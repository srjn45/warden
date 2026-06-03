import type { Session } from './types';

// waitingTransitions returns agents that are waiting_for_input in `next` but
// were NOT waiting_for_input in `prev` (including brand-new agents). These are
// the only events we surface as notifications.
export function waitingTransitions(prev: Session[], next: Session[]): Session[] {
  const wasWaiting = new Set(
    prev.filter((s) => s.status === 'waiting_for_input').map((s) => s.id),
  );
  return next.filter(
    (s) => s.status === 'waiting_for_input' && !wasWaiting.has(s.id),
  );
}
