import type { Session } from '../lib/types';
import { groupSessions } from '../lib/group';
import MiniTerminal from './MiniTerminal';
import BusyIdleBadge from './BusyIdleBadge';

// AgentGrid renders live thumbnail tiles for every agent, grouped by directory.
// Clicking a tile pins + focuses that agent. `lines` controls tile height; the
// Cockpit tab passes a larger value than the Overview mini-grid.
export default function AgentGrid({ sessions, onSelect, lines = 8 }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  lines?: number;
}) {
  if (sessions.length === 0) {
    return <p className="muted">No agents yet.</p>;
  }
  const groups = groupSessions(sessions);
  return (
    <div className="agent-grid-groups">
      {groups.map((g) => (
        <div key={g.dir} className="agent-grid-group">
          <div className="muted grid-group-head">{g.dir} ({g.sessions.length})</div>
          <div className="agent-grid">
            {g.sessions.map((s) => (
              <button key={s.id} className="grid-tile" onClick={() => onSelect(s.id)}>
                <div className="tile-head">
                  <b>{s.id}</b> <BusyIdleBadge status={s.status} />
                </div>
                <MiniTerminal id={s.id} lines={lines} />
              </button>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
