import type { Session } from '../lib/types';
import { needsAttention } from '../lib/attention';
import BusyIdleBadge from './BusyIdleBadge';

// AttentionQueue surfaces agents blocked on the user or failed. Clicking a card
// pins + focuses that agent.
export default function AttentionQueue({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  const items = needsAttention(sessions);
  if (items.length === 0) {
    return <p className="muted attn-empty">Nothing needs you right now. ✅</p>;
  }
  return (
    <div className="attn-queue">
      {items.map((s) => (
        <button key={s.id} className="attn-card" onClick={() => onSelect(s.id)}>
          <div className="attn-card-head">
            <b>{s.id}</b> <BusyIdleBadge status={s.status} />
          </div>
          <div className="muted attn-card-sub">{s.subject || s.prompt || s.type || '—'}</div>
        </button>
      ))}
    </div>
  );
}
