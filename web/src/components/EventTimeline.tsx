import type { AgentEvent } from '../lib/types';

export default function EventTimeline({ events }: { events: AgentEvent[] | null }) {
  const ev = (events ?? []).slice().reverse(); // newest first
  if (ev.length === 0) return <div className="muted">No events yet.</div>;
  return (
    <ul className="timeline">
      {ev.map((e, i) => (
        <li key={i}>
          <time>{e.ts ? new Date(e.ts).toLocaleTimeString() : ''}</time>{' '}
          <b>{e.type}</b>{e.detail && <span className="muted"> — {e.detail}</span>}
        </li>
      ))}
    </ul>
  );
}
