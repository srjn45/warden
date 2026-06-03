import type { Session } from '../lib/types';
import { deriveFleetStats } from '../lib/stats';
import { groupSessions } from '../lib/group';

// FleetStats is the at-a-glance health summary: status counters plus a per-dir
// agent count.
export default function FleetStats({ sessions }: { sessions: Session[] }) {
  const stats = deriveFleetStats(sessions);
  const groups = groupSessions(sessions);
  return (
    <div className="fleet-stats">
      <div className="stat"><span className="stat-n">{stats.total}</span> total</div>
      <div className="stat busy"><span className="stat-n">{stats.busy}</span> busy</div>
      <div className="stat attention"><span className="stat-n">{stats.waiting}</span> waiting</div>
      <div className="stat error"><span className="stat-n">{stats.errored}</span> errored</div>
      <div className="stat-dirs">
        {groups.map((g) => (
          <span key={g.dir} className="stat-dir muted">{g.dir} ({g.sessions.length})</span>
        ))}
      </div>
    </div>
  );
}
