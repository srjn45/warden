import { useEffect, useState } from 'react';
import type { Session, ApprovalView } from '../lib/types';
import { needsAttention } from '../lib/attention';
import { approvalActionFor } from '../lib/approvals';
import { listApprovals, approve } from '../lib/api';
import BusyIdleBadge from './BusyIdleBadge';

// AttentionQueue surfaces agents blocked on the user or failed. For recognized
// permission prompts it renders one-click option buttons (answer without
// attaching); unrecognized prompts and failures fall back to click-to-attach.
export default function AttentionQueue({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  const items = needsAttention(sessions);
  const [byId, setById] = useState<Record<string, ApprovalView>>({});

  const waitingKey = items.filter((s) => s.status === 'waiting_for_input').map((s) => s.id).join(',');
  useEffect(() => {
    let live = true;
    listApprovals().then((r) => {
      if (!live) return;
      const m: Record<string, ApprovalView> = {};
      for (const v of r.approvals) m[v.id] = v;
      setById(m);
    }).catch(() => { /* feature off or transient — fall back to cards */ });
    return () => { live = false; };
  }, [waitingKey]);

  if (items.length === 0) {
    return <p className="muted attn-empty">Nothing needs you right now. ✅</p>;
  }

  async function answer(id: string, option: number, fingerprint: string) {
    try {
      await approve(id, option, fingerprint);
    } catch {
      setById((prev: Record<string, ApprovalView>) => { const n = { ...prev }; delete n[id]; return n; });
    }
  }

  return (
    <div className="attn-queue">
      {items.map((s) => {
        const v = byId[s.id];
        const action = approvalActionFor(s.status, v);
        return (
          <div key={s.id} className="attn-card">
            <div className="attn-card-head">
              <b>{s.id}</b> <BusyIdleBadge status={s.status} />
            </div>
            <div className="muted attn-card-sub">
              {(v && (v.action || v.question)) || s.subject || s.prompt || s.type || '—'}
            </div>
            {action.kind === 'answer' ? (
              <div className="attn-options">
                {action.options.map((label, i) => (
                  <button key={i} className="attn-option" onClick={() => answer(s.id, i + 1, action.fingerprint)}>
                    {i + 1}. {label}
                  </button>
                ))}
              </div>
            ) : (
              <button className="attn-attach" onClick={() => onSelect(s.id)}>{action.label}</button>
            )}
          </div>
        );
      })}
    </div>
  );
}
