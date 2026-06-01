import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import AgentList from './AgentList';
import BusyIdleBadge from './BusyIdleBadge';

export default function Dashboard() {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    listSessions().then(setSessions).catch(() => { /* SSE will populate */ });
    const unsub = subscribeSessions(
      setSessions,
      () => setConnected(false),
      () => setConnected(true),
    );
    return unsub;
  }, []);

  const selected = sessions.find((s) => s.id === selectedId) ?? null;

  return (
    <div className="layout">
      <header className="topbar">
        <h1>agentctl</h1>
        <span className={connected ? 'conn ok' : 'conn down'}>
          {connected ? 'live' : 'reconnecting…'}
        </span>
      </header>
      <main className="main">
        <AgentList sessions={sessions} selectedId={selectedId} onSelect={setSelectedId} />
        {selected ? (
          <div className="detail">
            <h2>{selected.id} <BusyIdleBadge status={selected.status} /></h2>
            <p className="muted">detail view arrives in Phase G</p>
          </div>
        ) : (
          <div className="detail empty">Select an agent</div>
        )}
      </main>
    </div>
  );
}
