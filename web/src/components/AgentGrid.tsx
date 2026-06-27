import { useState } from 'react';
import type { Session } from '../lib/types';
import {
  groupSessionsBy, UNKNOWN_DIR,
  GROUP_BY_VALUES, GROUP_BY_LABELS,
  type GroupBy,
} from '../lib/group';
import MiniTerminal from './MiniTerminal';
import BusyIdleBadge from './BusyIdleBadge';
import ContextBadge from './ContextBadge';
import BackendLogo from './BackendLogo';
import QuickAddButton from './QuickAddButton';

// AgentGrid renders live thumbnail tiles for every agent, bucketed into titled
// panes. By default it groups by directory; when `groupControl` is set it shows
// a Group-by selector (Directory / Type / Status / Tag, #20) whose choice is
// saved to localStorage, and every group header is a collapse toggle. Each pane
// is a header bar (name + dim sub + count [+ quick-add]) over the tile grid.
// Clicking a tile pins + focuses that agent. `lines` controls tile height
// (Cockpit passes a larger value than the Overview mini-grid). When `onCreated`
// is provided, each directory pane (except the unknown-dir '—' group) shows a
// '+' that spawns a no-prompt agent in its dir.
//
// When `selectable` is set, each tile gains a checkbox; `selected` is the set of
// chosen ids and `onToggleSelect(id, shift)` toggles one (shift = range-select).
// Selection drives the bulk action bar (#21 batch operations).

const GROUP_KEY = 'warden.grouping';

function loadGroupBy(): GroupBy {
  try {
    const raw = localStorage.getItem(GROUP_KEY);
    if (raw && (GROUP_BY_VALUES as string[]).includes(raw)) return raw as GroupBy;
  } catch { /* unavailable storage */ }
  return 'dir';
}

export default function AgentGrid({ sessions, onSelect, lines = 8, onCreated, selectable, selected, onToggleSelect, groupControl }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  lines?: number;
  onCreated?: (id: string) => void;
  selectable?: boolean;
  selected?: Set<string>;
  onToggleSelect?: (id: string, shift: boolean) => void;
  groupControl?: boolean;
}) {
  const [groupBy, setGroupBy] = useState<GroupBy>(loadGroupBy);
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());

  // Group control is only meaningful when shown; otherwise honor the saved
  // preference but stick to the directory default for the compact mini-grid.
  const mode: GroupBy = groupControl ? groupBy : 'dir';

  function chooseGroup(g: GroupBy) {
    setGroupBy(g);
    setCollapsed(new Set());
    try { localStorage.setItem(GROUP_KEY, g); } catch { /* unavailable storage */ }
  }

  function toggleCollapse(key: string) {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  if (sessions.length === 0) {
    return <p className="muted">No agents yet.</p>;
  }
  const groups = groupSessionsBy(sessions, mode);
  return (
    <>
      {groupControl && (
        <div className="grid-group-toolbar" role="group" aria-label="Group agents by">
          <span className="muted">Group by</span>
          {GROUP_BY_VALUES.map((g) => (
            <button
              key={g}
              className={`group-by-btn${mode === g ? ' active' : ''}`}
              aria-pressed={mode === g}
              onClick={() => chooseGroup(g)}
            >
              {GROUP_BY_LABELS[g]}
            </button>
          ))}
        </div>
      )}
      <div className="agent-grid-groups">
        {groups.map((g) => {
          const isCollapsed = collapsed.has(g.key);
          return (
            <div key={g.key} className="agent-grid-group">
              <div className="grid-group-bar">
                <button
                  className="grid-group-toggle"
                  aria-expanded={!isCollapsed}
                  aria-label={`${isCollapsed ? 'Expand' : 'Collapse'} ${g.label}`}
                  onClick={() => toggleCollapse(g.key)}
                >
                  <span className="grid-group-caret">{isCollapsed ? '▸' : '▾'}</span>
                  <span className="grid-group-name">{g.label}</span>
                  {g.sub && <span className="grid-group-path">{g.sub}</span>}
                </button>
                <span className="grid-group-count">{g.sessions.length}</span>
                {onCreated && g.dir && g.dir !== UNKNOWN_DIR && (
                  <QuickAddButton dir={g.dir} onCreated={onCreated} />
                )}
              </div>
              {!isCollapsed && (
                <div className="agent-grid">
                  {g.sessions.map((s) => {
                    const isSel = selected?.has(s.id) ?? false;
                    return (
                      <div key={s.id} className={`grid-tile-wrap${isSel ? ' selected' : ''}`}>
                        {selectable && (
                          <input
                            type="checkbox"
                            className="tile-select"
                            checked={isSel}
                            aria-label={`Select ${s.id}`}
                            onChange={() => { /* controlled via onClick */ }}
                            onClick={(e) => onToggleSelect?.(s.id, e.shiftKey)}
                          />
                        )}
                        <button className="grid-tile" onClick={() => onSelect(s.id)}>
                          <div className="tile-head">
                            <BackendLogo backend={s.backend} />
                            <b>{s.id}</b> <BusyIdleBadge status={s.status} exitCode={s.exit_code} />
                            <ContextBadge tokens={s.context_tokens} state={s.context_state} />
                          </div>
                          <MiniTerminal id={s.id} lines={lines} />
                        </button>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          );
        })}
      </div>
    </>
  );
}
