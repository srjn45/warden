import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { deriveFleetStats } from '../lib/stats';
import { groupSessions } from '../lib/group';
import { getPressure } from '../lib/api';
import { gaugeClass, gaugeLabel, type PressureStatus } from '../lib/pressure';

// FleetStats is the at-a-glance health summary: status counters plus a per-dir
// agent count.
export default function FleetStats({ sessions }: { sessions: Session[] }) {
  const stats = deriveFleetStats(sessions);
  const groups = groupSessions(sessions);

  const [press, setPress] = useState<PressureStatus | null>(null);
  useEffect(() => {
    let alive = true;
    const tick = () => { getPressure().then((p) => { if (alive) setPress(p); }).catch(() => {}); };
    tick();
    const h = setInterval(tick, 5000);
    return () => { alive = false; clearInterval(h); };
  }, []);

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
      {press && <span className={`pressure-gauge ${gaugeClass(press.level)}`}>{gaugeLabel(press)}</span>}
    </div>
  );
}
