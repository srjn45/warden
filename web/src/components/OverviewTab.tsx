import { useEffect, useMemo, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { filterSessions } from '../lib/search';
import AttentionQueue from './AttentionQueue';
import ConflictsPanel from './ConflictsPanel';
import FleetStats from './FleetStats';
import ResourcesPanel from './ResourcesPanel';
import QuickSpawn from './QuickSpawn';
import AgentGrid from './AgentGrid';
import ActivityFeed from './ActivityFeed';

// OverviewTab composes the home-screen sections: attention queue, fleet stats,
// quick spawn paired with the recent activity feed, and the all-agents mini-grid.
// The mini-grid carries a full-text search box (#28) that filters the live fleet
// client-side across name/id/type/subject/prompt/branch/pane text.
export default function OverviewTab({ sessions, onSelect, focusSignal }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  focusSignal?: number; // bumped by the `/` shortcut to focus the search box
}) {
  const [query, setQuery] = useState('');
  const shown = useMemo(() => filterSessions(sessions, query), [sessions, query]);
  const searchRef = useRef<HTMLInputElement>(null);

  // The `/` keyboard shortcut switches to this tab and bumps focusSignal; focus
  // (and select, so a new query overwrites) the search box when it does.
  useEffect(() => {
    if (focusSignal) { searchRef.current?.focus(); searchRef.current?.select(); }
  }, [focusSignal]);
  return (
    <div className="overview">
      <section className="card">
        <h3>Needs you</h3>
        <AttentionQueue sessions={sessions} onSelect={onSelect} />
      </section>
      <section className="card">
        <h3>File conflicts</h3>
        <ConflictsPanel onSelect={onSelect} />
      </section>
      <section className="card">
        <h3>Fleet</h3>
        <FleetStats sessions={sessions} />
      </section>
      <section className="card">
        <h3>Resources</h3>
        <ResourcesPanel />
      </section>
      <section className="card">
        <h3>Quick spawn</h3>
        <QuickSpawn onCreated={onSelect} />
      </section>
      <section className="card overview-activity">
        <h3>Recent activity</h3>
        <ActivityFeed sessions={sessions} />
      </section>
      <section className="card overview-grid">
        <div className="grid-head">
          <h3>All agents</h3>
          <input
            ref={searchRef}
            type="search"
            className="agent-search"
            placeholder="search agents…  ( / )"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Search agents"
          />
          {query && <span className="muted">{shown.length} / {sessions.length}</span>}
        </div>
        <AgentGrid sessions={shown} onSelect={onSelect} lines={6} />
      </section>
    </div>
  );
}
