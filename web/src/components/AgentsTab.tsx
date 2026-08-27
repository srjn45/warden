import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { terminate } from '../lib/api';
import AgentTab from './AgentTab';
import BusyIdleBadge from './BusyIdleBadge';

export default function AgentsTab({ agents }: { agents: Session[] }) {
  const [selected, setSelected] = useState<string | null>(null);

  // Keep the selection valid as the live list changes: default to the first
  // agent, and when the selected one disappears fall back to the first.
  useEffect(() => {
    if (agents.length === 0) {
      if (selected !== null) setSelected(null);
      return;
    }
    if (selected === null || !agents.some((a) => a.id === selected)) {
      setSelected(agents[0].id);
    }
  }, [agents, selected]);

  function close(id: string) {
    // Fire-and-forget: the list reconciles over SSE when the session ends.
    terminate(id).catch(() => { /* already gone / will reconcile */ });
  }

  const selectedAgent = selected ? agents.find((a) => a.id === selected) : null;

  return (
    <div className="agents">
      <aside className="agents-list card">
        <div className="agents-head">
          <h3>Agents</h3>
        </div>
        {agents.length === 0
          ? <p className="muted">No agents running.</p>
          : (
            <ul>
              {agents.map((a) => (
                <li key={a.id} className={`agents-row${a.id === selected ? ' on' : ''}`}>
                  <button className="agents-pick" onClick={() => setSelected(a.id)}>
                    <span className="agents-name">{a.id}</span>
                    <BusyIdleBadge status={a.status} exitCode={a.exit_code} />
                  </button>
                  <button className="agents-close" title="Terminate agent" onClick={() => close(a.id)}>✕</button>
                </li>
              ))}
            </ul>
          )}
      </aside>
      <section className="agents-view">
        {selectedAgent
          ? <AgentTab key={selectedAgent.id} session={selectedAgent} onClosed={() => setSelected(null)} />
          : <div className="detail empty">No agent selected.</div>}
      </section>
    </div>
  );
}
