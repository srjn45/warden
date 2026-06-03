import { useEffect, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { getOutput, sendInput } from '../lib/api';
import EventTimeline from './EventTimeline';
import TerminateControls from './TerminateControls';
import BusyIdleBadge from './BusyIdleBadge';

export default function AgentDetail({ session, onClosed }: { session: Session; onClosed: () => void }) {
  const [output, setOutput] = useState('');
  const [msg, setMsg] = useState('');
  const [sending, setSending] = useState(false);
  const preRef = useRef<HTMLPreElement>(null);

  // Poll the live terminal pane every 2s while this detail is open.
  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const o = await getOutput(session.id, 200);
        if (alive) setOutput(o);
      } catch { /* session may have ended; SSE will drop it from the list */ }
    };
    poll();
    const t = setInterval(poll, 2000);
    return () => { alive = false; clearInterval(t); };
  }, [session.id]);

  useEffect(() => {
    if (preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight;
  }, [output]);

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
    <div className="detail">
      <div className="detail-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} /></h2>
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || '—'}
          {session.subject && ` · ${session.subject}`}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      {session.prompt && (
        <section>
          <h3>Prompt</h3>
          <p className="muted" style={{ whiteSpace: 'pre-wrap' }}>{session.prompt}</p>
        </section>
      )}

      <section>
        <h3>Live output</h3>
        <pre className="pane" ref={preRef}>{output || '(no output captured yet)'}</pre>
      </section>

      <section className="sendbox">
        <input
          value={msg}
          onChange={(e) => setMsg(e.target.value)}
          placeholder="Send a message to this agent…"
          onKeyDown={(e) => { if (e.key === 'Enter') send(); }}
        />
        <button disabled={sending} onClick={send}>Send</button>
      </section>

      <section>
        <h3>History</h3>
        <EventTimeline events={session.events} />
        {session.worktree && (
          <p className="muted">Attach in a terminal: <code>agentctl attach {session.id}</code></p>
        )}
      </section>
    </div>
  );
}
