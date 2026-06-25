import { useEffect, useMemo, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { groupSessions } from '../lib/group';
import AgentGrid from './AgentGrid';
import BulkActionBar from './BulkActionBar';

// CockpitTab is the full-size live grid (taller tiles than the Overview
// mini-grid). Clicking a pane pins + focuses that agent; the per-pane '+'
// (wired via onCreated) spawns a new agent in that pane's directory.
//
// The Cockpit is also where batch operations (#21) live: each tile carries a
// checkbox, and selecting one or more agents reveals the bulk action bar.
export default function CockpitTab({ sessions, onSelect, onCreated }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  onCreated: (id: string) => void;
}) {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const lastRef = useRef<string | null>(null);

  // Flat id order matching the grid's render order, so shift-select spans the
  // visible range across directory groups.
  const orderedIds = useMemo(
    () => groupSessions(sessions).flatMap((g) => g.sessions.map((s) => s.id)),
    [sessions],
  );

  // Drop selections for agents that have ended (pruned from the live list).
  useEffect(() => {
    setSelected((prev) => {
      const alive = new Set(orderedIds);
      let changed = false;
      const next = new Set<string>();
      for (const id of prev) {
        if (alive.has(id)) next.add(id); else changed = true;
      }
      return changed ? next : prev;
    });
  }, [orderedIds]);

  function toggle(id: string, shift: boolean) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (shift && lastRef.current) {
        const i = orderedIds.indexOf(lastRef.current);
        const j = orderedIds.indexOf(id);
        if (i >= 0 && j >= 0) {
          const [lo, hi] = i < j ? [i, j] : [j, i];
          for (let k = lo; k <= hi; k++) next.add(orderedIds[k]);
        }
      } else if (next.has(id)) {
        next.delete(id);
      } else {
        next.add(id);
      }
      return next;
    });
    lastRef.current = id;
  }

  return (
    <div className="cockpit">
      <AgentGrid
        sessions={sessions}
        onSelect={onSelect}
        lines={14}
        onCreated={onCreated}
        selectable
        selected={selected}
        onToggleSelect={toggle}
      />
      {selected.size > 0 && (
        <BulkActionBar selected={[...selected]} onClear={() => setSelected(new Set())} />
      )}
    </div>
  );
}
