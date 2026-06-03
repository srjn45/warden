import { useState } from 'react';
import type { Session } from '../lib/types';
import AttachTerminal from './AttachTerminal';
import EventTimeline from './EventTimeline';
import TerminateControls from './TerminateControls';
import BusyIdleBadge from './BusyIdleBadge';

// AgentTab is the focused single-agent view: a fully interactive terminal
// (real tmux attach), collapsible details + event timeline, and teardown controls.
export default function AgentTab({ session, onClosed }: {
  session: Session;
  onClosed: () => void;
}) {
  const [showDetails, setShowDetails] = useState(false);

  return (
    <div className="agent-tab">
      <div className="agent-tab-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} /></h2>
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || session.repo || '—'}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      <AttachTerminal id={session.id} />

      <button className="details-toggle" onClick={() => setShowDetails((v) => !v)}>
        {showDetails ? '▾ Hide details' : '▸ Details & history'}
      </button>
      {showDetails && (
        <section className="agent-details">
          {session.prompt && (
            <p className="muted" style={{ whiteSpace: 'pre-wrap' }}>{session.prompt}</p>
          )}
          <EventTimeline events={session.events} />
          {session.worktree && (
            <p className="muted">Attach in a terminal: <code>agentctl attach {session.id}</code></p>
          )}
        </section>
      )}
    </div>
  );
}
