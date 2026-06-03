import { useState } from 'react';
import type { Session } from '../lib/types';
import { sendInput } from '../lib/api';
import Terminal from './Terminal';
import EventTimeline from './EventTimeline';
import TerminateControls from './TerminateControls';
import BusyIdleBadge from './BusyIdleBadge';

// AgentTab is the focused single-agent view: a live colored terminal, a send
// box, collapsible details + event timeline, and teardown controls.
export default function AgentTab({ session, onClosed }: {
  session: Session;
  onClosed: () => void;
}) {
  const [msg, setMsg] = useState('');
  const [sending, setSending] = useState(false);
  const [showDetails, setShowDetails] = useState(false);

  async function send() {
    if (!msg.trim()) return;
    setSending(true);
    try {
      await sendInput(session.id, msg);
      setMsg('');
    } catch { /* surfaced via list status / SSE */ } finally {
      setSending(false);
    }
  }

  return (
    <div className="agent-tab">
      <div className="agent-tab-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} /></h2>
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || session.repo || '—'}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      <Terminal id={session.id} />

      <section className="sendbox">
        <input
          value={msg}
          onChange={(e) => setMsg(e.target.value)}
          placeholder="Send a message to this agent…"
          onKeyDown={(e) => { if (e.key === 'Enter') send(); }}
        />
        <button disabled={sending} onClick={send}>Send</button>
      </section>

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
