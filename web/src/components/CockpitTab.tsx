import { useEffect, useMemo, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { visibleAgentIds, type ProjectTree } from '../lib/tree';
import { partitionByKind } from '../lib/kind';
import AgentGrid from './AgentGrid';
import BulkActionBar from './BulkActionBar';
import FleetStats from './FleetStats';

// CockpitTab is the full-size live grid and the default home view. A slim Fleet
// header (FleetStats, moved here from the former Overview) sits above the grid;
// clicking a pane pins + focuses that agent; the per-pane '+' (wired via
// onCreated) spawns a new agent in that pane's directory.
//
// The Cockpit is also where batch operations (#21) live: each tile carries a
// checkbox, and selecting one or more agents reveals the bulk action bar.
export default function CockpitTab({ sessions, tree, treeError, stale, onSelect, onCreated, onTerminalSelect }: {
  sessions: Session[];
  tree: ProjectTree | null;
  treeError: string | null;
  stale: boolean;
  onTerminalSelect: (id: string) => void;
  onSelect: (id: string) => void;
  onCreated: (id: string) => void;
}) {
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const lastRef = useRef<string | null>(null);

  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const { agents } = partitionByKind(sessions);
  const orderedIds = useMemo(
    () => visibleAgentIds(tree?.roots ?? [], new Set(sessions.filter((s) => s.kind !== 'terminal').map((s) => s.id)), collapsed),
    [tree, sessions, collapsed],
  );

  // Drop selections that are no longer visible or available in the hierarchy.
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
      <section className="card cockpit-fleet">
        <h3>Fleet</h3>
        <FleetStats sessions={agents} />
      </section>
      {treeError && <p className="warn" role="status">{treeError}</p>}
      {tree && stale && <p className="muted" role="status">Project hierarchy may be out of date. Reconnecting…</p>}
      {!tree && !treeError && <p className="muted" role="status">Loading projects…</p>}
      {tree && <AgentGrid
        tree={tree}
        sessions={sessions}
        collapsed={collapsed}
        onToggleCollapse={(id) => setCollapsed((prev) => {
          const next = new Set(prev);
          if (next.has(id)) next.delete(id); else next.add(id);
          return next;
        })}
        onTerminalSelect={onTerminalSelect}
        onSelect={onSelect}
        lines={14}
        onCreated={onCreated}
        selectable
        selected={selected}
        onToggleSelect={toggle}
      />}
      {selected.size > 0 && (
        <BulkActionBar selected={[...selected]} onClear={() => setSelected(new Set())} />
      )}
    </div>
  );
}
