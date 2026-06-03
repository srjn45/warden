import type { Session } from '../lib/types';
import { mergeEvents } from '../lib/activity';

// ActivityFeed is a merged, newest-first event stream across all agents.
export default function ActivityFeed({ sessions }: { sessions: Session[] }) {
  const items = mergeEvents(sessions);
  if (items.length === 0) return <p className="muted">No activity yet.</p>;
  return (
    <ul className="timeline activity-feed">
      {items.map((e, i) => (
        <li key={i}>
          <time>{e.ts ? new Date(e.ts).toLocaleTimeString() : ''}</time>{' '}
          <code className="muted">{e.id}</code>{' '}
          <b>{e.type}</b>{e.detail && <span className="muted"> — {e.detail}</span>}
        </li>
      ))}
    </ul>
  );
}
