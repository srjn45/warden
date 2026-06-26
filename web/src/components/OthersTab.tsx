import type { Session } from '../lib/types';
import AttentionQueue from './AttentionQueue';
import ConflictsPanel from './ConflictsPanel';
import ActivityFeed from './ActivityFeed';

// OthersTab is the catch-all landing surface (formerly Overview). After the
// rewamp it holds only the cross-cutting cards: the attention queue (Needs you),
// file conflicts, and the recent activity feed. Fleet now lives in the Cockpit
// header, and the redundant Quick-spawn / All-agents mini-grid are gone — the
// top-right "+ New agent" button is the single spawn path. New or not-yet-homed
// widgets land here until they earn a dedicated home.
export default function OthersTab({ sessions, onSelect }: {
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
        <h3>File conflicts</h3>
        <ConflictsPanel onSelect={onSelect} />
      </section>
      <section className="card overview-activity">
        <h3>Recent activity</h3>
        <ActivityFeed sessions={sessions} />
      </section>
    </div>
  );
}
