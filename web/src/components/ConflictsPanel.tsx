import { useEffect, useState } from 'react';
import type { Conflict, ConflictAgent } from '../lib/types';
import { listConflicts } from '../lib/api';

// ConflictsPanel surfaces files edited by two or more active agents at once —
// the Web view of `warden collab conflicts`. It polls every 5s (conflicts are
// not on the SSE session channel) and stays quiet when nothing overlaps.
// Read-only: warnings are also delivered to each agent's inbox by the daemon.
export default function ConflictsPanel({ onSelect }: { onSelect: (id: string) => void }) {
  const [conflicts, setConflicts] = useState<Conflict[]>([]);

  useEffect(() => {
    let on = true;
    const load = () => {
      listConflicts().then((c) => { if (on) setConflicts(c); }).catch(() => { /* keep last */ });
    };
    load();
    const t = setInterval(load, 5000);
    return () => { on = false; clearInterval(t); };
  }, []);

  if (conflicts.length === 0) {
    return <div className="empty">No file conflicts — agents are working on distinct files.</div>;
  }

  return (
    <ul className="conflict-list">
      {conflicts.map((c) => (
        <li key={c.file} className="conflict">
          <span className="conflict-file">{c.file}</span>
          <span className="conflict-agents">
            {c.agents.map((a, i) => (
              <span key={a.id}>
                {i > 0 && <span className="conflict-sep">, </span>}
                <button className="conflict-agent" onClick={() => onSelect(a.id)} title={`Open ${a.id}`}>
                  {agentLabel(a)}
                </button>
              </span>
            ))}
          </span>
        </li>
      ))}
    </ul>
  );
}

function agentLabel(a: ConflictAgent): string {
  return a.name ? `${a.id} (${a.name})` : a.id;
}
