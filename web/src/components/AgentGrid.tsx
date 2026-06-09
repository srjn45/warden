import type { Session } from '../lib/types';
import { groupSessions, baseName, UNKNOWN_DIR } from '../lib/group';
import MiniTerminal from './MiniTerminal';
import BusyIdleBadge from './BusyIdleBadge';
import QuickAddButton from './QuickAddButton';

// AgentGrid renders live thumbnail tiles for every agent, grouped by directory.
// Each directory group is a titled pane: a header bar (folder name + dim path +
// count [+ quick-add]) over the tile grid. Clicking a tile pins + focuses that
// agent. `lines` controls tile height (Cockpit passes a larger value than the
// Overview mini-grid). When `onCreated` is provided, each pane (except the
// unknown-dir '—' group) shows a '+' that spawns a no-prompt agent in its dir.
export default function AgentGrid({ sessions, onSelect, lines = 8, onCreated }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  lines?: number;
  onCreated?: (id: string) => void;
}) {
  if (sessions.length === 0) {
    return <p className="muted">No agents yet.</p>;
  }
  const groups = groupSessions(sessions);
  return (
    <div className="agent-grid-groups">
      {groups.map((g) => (
        <div key={g.dir} className="agent-grid-group">
          <div className="grid-group-bar">
            <span className="grid-group-name">{baseName(g.dir)}</span>
            <span className="grid-group-path">{g.dir}</span>
            <span className="grid-group-count">{g.sessions.length}</span>
            {onCreated && g.dir !== UNKNOWN_DIR && (
              <QuickAddButton dir={g.dir} onCreated={onCreated} />
            )}
          </div>
          <div className="agent-grid">
            {g.sessions.map((s) => (
              <button key={s.id} className="grid-tile" onClick={() => onSelect(s.id)}>
                <div className="tile-head">
                  <b>{s.id}</b> <BusyIdleBadge status={s.status} exitCode={s.exit_code} />
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
