import type { Session } from '../lib/types';
import AttentionQueue from './AttentionQueue';
import FleetStats from './FleetStats';
import QuickSpawn from './QuickSpawn';
import AgentGrid from './AgentGrid';
import ActivityFeed from './ActivityFeed';

// OverviewTab composes the home-screen sections: attention queue, fleet stats,
// quick spawn paired with the recent activity feed, and the all-agents mini-grid.
export default function OverviewTab({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  return (
    <div className="overview">
      <section className="card">
        <h3>Needs you</h3>
        <AttentionQueue sessions={sessions} onSelect={onSelect} />
      </section>
      <section className="card">
        <h3>Fleet</h3>
        <FleetStats sessions={sessions} />
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
        <h3>All agents</h3>
        <AgentGrid sessions={sessions} onSelect={onSelect} lines={6} />
      </section>
    </div>
  );
}
