import type { Session } from '../lib/types';
import BusyIdleBadge from './BusyIdleBadge';

function age(iso: string): string {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  const m = Math.floor(ms / 60000);
  if (m < 1) return '<1m';
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h${m % 60}m`;
}

function lastDetail(s: Session): string {
  const ev = s.events;
  return ev && ev.length ? ev[ev.length - 1].detail : '';
}

export default function AgentList({ sessions, selectedId, onSelect }: {
  sessions: Session[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  if (sessions.length === 0) {
    return <div className="list empty">No agents yet. Click "+ New agent".</div>;
  }
  return (
    <div className="list">
      <table>
        <thead>
          <tr><th>ID</th><th>Type</th><th>State</th><th>Status</th><th>Age</th><th>Detail</th></tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <tr key={s.id} className={s.id === selectedId ? 'sel' : ''} onClick={() => onSelect(s.id)}>
              <td>{s.id}</td>
              <td>{s.type || <span className="muted">classifying…</span>}</td>
              <td><BusyIdleBadge status={s.status} /></td>
              <td>{s.status}</td>
              <td>{age(s.updated_at)}</td>
              <td className="muted">{lastDetail(s)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
