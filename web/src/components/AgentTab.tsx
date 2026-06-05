import { useState } from 'react';
import type { Session, Digest } from '../lib/types';
import { getDigest } from '../lib/api';
import { fileLabel, hasFiles } from '../lib/digest';
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
  const [digest, setDigest] = useState<Digest | null>(null);
  const [digestBusy, setDigestBusy] = useState(false);
  const [digestErr, setDigestErr] = useState<string | null>(null);

  async function loadDigest() {
    setDigestBusy(true); setDigestErr(null);
    try {
      setDigest(await getDigest(session.id));
    } catch (e) {
      setDigestErr(e instanceof Error ? e.message : String(e));
    } finally {
      setDigestBusy(false);
    }
  }

  return (
    <div className="agent-tab">
      <div className="agent-tab-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} />{session.supervised && <span className="supervised-pill">supervised</span>}</h2>
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || session.repo || '—'}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      <AttachTerminal id={session.id} />

      <div className="agent-tab-actions">
        <button className="details-toggle" onClick={() => setShowDetails((v) => !v)}>
          {showDetails ? '▾ Hide details' : '▸ Details & history'}
        </button>
        <button className="details-toggle" onClick={loadDigest} disabled={digestBusy}>
          {digestBusy ? '⏳ Generating digest…' : '✦ Digest'}
        </button>
      </div>
      {digestErr && <span className="warn">{digestErr}</span>}
      {digest && (
        <section className="digest-panel">
          <p className="digest-summary" style={{ whiteSpace: 'pre-wrap' }}>{digest.summary}</p>
          <pre className="digest-files">
            {hasFiles(digest)
              ? digest.files!.map(fileLabel).join('\n')
              : '(no files touched)'}
          </pre>
          <div className="muted digest-meta">
            branch: {digest.branch || '—'} · turns: {digest.turns} · status: {digest.status}
          </div>
        </section>
      )}
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
