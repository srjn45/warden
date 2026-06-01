import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import AgentList from './AgentList';
import AgentDetail from './AgentDetail';
import NewAgentModal from './NewAgentModal';

export default function Dashboard() {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);
  const [showCreate, setShowCreate] = useState(false);

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
        <button onClick={() => setShowCreate(true)}>+ New agent</button>
      </header>
      <main className="main">
        <AgentList sessions={sessions} selectedId={selectedId} onSelect={setSelectedId} />
        {selected
          ? <AgentDetail session={selected} onClosed={() => setSelectedId(null)} />
          : <div className="detail empty">Select an agent</div>}
      </main>
      {showCreate && (
        <NewAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(id) => { setShowCreate(false); setSelectedId(id); }}
        />
      )}
    </div>
  );
}
