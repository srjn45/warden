import type { Session } from './types';

export interface ActivityItem {
  id: string;     // agent id the event belongs to
  ts: string;
  type: string;
  detail: string;
}

// mergeEvents flattens every agent's event list into one feed, newest first,
// capped at `limit`. Each item is tagged with its agent id.
export function mergeEvents(sessions: Session[], limit = 50): ActivityItem[] {
  const items: ActivityItem[] = [];
  for (const s of sessions) {
    for (const e of s.events ?? []) {
      items.push({ id: s.id, ts: e.ts, type: e.type, detail: e.detail });
    }
  }
  items.sort((a, b) => new Date(b.ts).getTime() - new Date(a.ts).getTime());
  return items.slice(0, limit);
}
