import { useEffect, useState } from 'react';
import type { Session, Digest, RuntimeView, TraceRecord } from '../lib/types';
import { getDigest, getSessionRuntime, getSessionDecisions } from '../lib/api';
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

  const [rt, setRt] = useState<RuntimeView | null>(null);
  const [trace, setTrace] = useState<TraceRecord[]>([]);
  useEffect(() => {
    let live = true;
    const load = () => {
      getSessionRuntime(session.id).then((v) => live && setRt(v)).catch(() => {});
      getSessionDecisions(session.id, 20).then((v) => live && setTrace(v)).catch(() => {});
    };
    load();
    const t = setInterval(load, 5000);
    return () => { live = false; clearInterval(t); };
  }, [session.id]);

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
        <h2>{session.id} <BusyIdleBadge status={session.status} exitCode={session.exit_code} />{session.supervised && <span className="supervised-pill">supervised</span>}</h2>
        <code className="muted">
          role: {session.role || session.type || 'general'} · dir: {session.workdir || session.repo || '—'}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      {rt?.danger && (
        <div className="danger-banner" role="alert">{rt.danger_banner || 'DANGER: dangerously-execute-all'}</div>
      )}
      {rt && (
        <section className="runtime-card" aria-label="PTY runtime">
          <b>runtime {rt.mode}</b>
          {rt.supervised ? (
            <span className="muted">
              {' '}state {rt.state}{rt.reason ? ` (${rt.reason})` : ''} · input {rt.input_owner || '—'} · route {rt.route || '—'} · posture {rt.posture}
              {rt.evidence && rt.evidence.length > 0 && <> · evidence {rt.evidence.join('; ')}</>}
            </span>
          ) : <span className="muted"> not supervised</span>}
        </section>
      )}

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
          {trace.length > 0 && (
            <table className="decision-trace">
              <thead><tr><th>time</th><th>outcome</th><th>route</th><th>summary</th><th>reason</th></tr></thead>
              <tbody>
                {trace.map((r) => (
                  <tr key={r.id}>
                    <td>{new Date(r.time).toLocaleTimeString()}</td>
                    <td>{r.outcome}{r.danger ? ' ⚠' : ''}</td>
                    <td>{r.route || '—'}</td>
                    <td>{r.permission?.text || r.action || '—'}</td>
                    <td>{r.reason || ''}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {session.worktree && (
            <p className="muted">Attach in a terminal: <code>warden attach {session.id}</code></p>
          )}
        </section>
      )}
    </div>
  );
}
