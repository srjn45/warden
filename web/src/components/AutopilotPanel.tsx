import { useCallback, useEffect, useState } from 'react';
import {
  getAutopilot, setAutopilot,
	controlAutopilotRun,
  AutopilotPreflightError,
  type AutopilotStatus, type AutopilotRun,
} from '../lib/api';
import type { Session } from '../lib/types';

// AutopilotPanel is a modal panel for the autopilot toggle and status view.
// Opens from the "⚙ autopilot" button in the AttentionBar.
export default function AutopilotPanel({ onClose, liveStatus, sessions, stale }: { onClose: () => void; liveStatus?: AutopilotStatus | null; sessions?: Session[]; stale?: boolean }) {
  const [status, setStatus] = useState<AutopilotStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [preflightFailures, setPreflightFailures] = useState<string[] | null>(null);

  const refresh = useCallback(() => {
    getAutopilot().then((s) => {
      setStatus({ ...s, runs: s.runs ?? [] });
    }).catch(() => { /* silent — daemon may not be running */ });
  }, []);

  useEffect(() => {
    refresh();
    const h = setInterval(refresh, 5000);
    return () => clearInterval(h);
  }, [refresh]);

	useEffect(() => { if (liveStatus) setStatus(liveStatus); }, [liveStatus]);

  async function toggle() {
    if (!status) return;
    setLoading(true);
    setError(null);
    setPreflightFailures(null);
    try {
      const next = await setAutopilot(!status.enabled);
      setStatus({ ...next, runs: next.runs ?? [] });
    } catch (e) {
      if (e instanceof AutopilotPreflightError) {
        setPreflightFailures(e.failures);
      } else {
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setLoading(false);
    }
  }

  const enabled = status?.enabled ?? false;
  const runs = status?.runs ?? [];

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal autopilot-panel" onClick={(e) => e.stopPropagation()}>
        <header className="autopilot-panel-head">
          <h2>Autopilot</h2>
		  {stale && status && <span className="warn">stale · showing last update</span>}
          <button className="context-drawer-close" title="Close" onClick={onClose}>✕</button>
        </header>

        {/* Toggle row */}
        <div className="autopilot-toggle-row">
          <span className={`autopilot-state ${enabled ? 'on' : 'off'}`}>
            {status == null ? '…' : enabled ? 'on' : 'off'}
          </span>
          <button
            className={`autopilot-toggle-btn ${enabled ? 'danger' : 'new-btn'}`}
            disabled={loading || status == null}
            onClick={toggle}
          >
            {loading ? '…' : enabled ? 'Disable' : 'Enable'}
          </button>
        </div>

        {/* Preflight failures (409) */}
        {preflightFailures != null && (
          <div className="autopilot-preflight-error">
            <strong>Enable failed — fix these issues:</strong>
            <ul>
              {preflightFailures.map((f, i) => <li key={i}>{f}</li>)}
            </ul>
            <p className="autopilot-init-hint">
              Run <code>warden autopilot init</code> to scaffold a plan file and config block.
            </p>
          </div>
        )}

        {/* Generic error */}
        {error != null && (
          <p className="warn">{error}</p>
        )}

        {/* Run list */}
        {runs.length === 0 && enabled && (
          <p className="muted">No active runs.</p>
        )}
        {runs.length === 0 && !enabled && status != null && (
          <p className="muted">
            Disabled. Run <code>warden autopilot init</code> in your repo, then enable here or with <code>warden autopilot on</code>.
          </p>
        )}
		{runs.map((r) => <RunCard key={r.run_id} run={r} sessions={sessions ?? []} onChanged={refresh} onError={setError} />)}
      </div>
    </div>
  );
}

function RunCard({ run, sessions, onChanged, onError }: { run: AutopilotRun; sessions: Session[]; onChanged: () => void; onError: (message: string | null) => void }) {
	const [busy, setBusy] = useState(false);
	const agents = sessions.filter((s) => s.tags?.includes(`run:${run.run_id}`));
	async function act(action: 'pause'|'resume'|'stop') {
		onError(null); setBusy(true);
		try { await controlAutopilotRun(run.run_id, action); onChanged(); }
		catch (e) { onError(e instanceof Error ? e.message : String(e)); }
		finally { setBusy(false); }
	}
  return (
    <section className="autopilot-run-card">
      <div className="autopilot-run-head">
        <span className={`autopilot-run-state state-${run.state}`}>{run.state}</span>
		<span className="muted" title={run.repo}>{run.name || run.plan_file}</span>
        <span className="muted">gate: {run.gate}</span>
      </div>
	  <div className="autopilot-run-controls">
		<button disabled={busy || !['active','paused','degraded','healing'].includes(run.state)} onClick={() => act(run.state === 'paused' ? 'resume' : 'pause')}>{run.state === 'paused' ? 'Resume' : 'Pause'}</button>
		<button className="danger" disabled={busy || ['stopped','complete'].includes(run.state)} onClick={() => act('stop')}>Stop</button>
	  </div>

      {run.brain && (
        <div className="autopilot-run-brain">
          <span>brain: {run.brain.backend} ({run.brain.tier})</span>
          {run.brain.last_heartbeat && (
            <span className="muted" title={run.brain.last_heartbeat}>
              ♡ {relativeTime(run.brain.last_heartbeat)}
            </span>
          )}
          {run.brain.context_level && run.brain.context_level !== 'ok' && (
            <span className="warn">ctx: {run.brain.context_level}</span>
          )}
        </div>
      )}

      <div className="autopilot-run-stats">
        <span>workers: {run.workers_in_flight}</span>
        <span>tasks — pending: {run.tasks.pending} · active: {run.tasks.in_progress} · landed: {run.tasks.landed}</span>
        {run.landed_total > 0 && <span>total landed: {run.landed_total}</span>}
      </div>
	  <div className="autopilot-task-list">
		{(run.plan_tasks ?? []).map((t) => <div key={t.id} className={`autopilot-task task-${t.status}`}><span>{t.status === 'done' ? '✓' : t.status === 'active' ? '◐' : t.status === 'failed' ? '✗' : '○'}</span><strong>{t.id}</strong><span className="muted">{t.prompt}</span></div>)}
	  </div>
	  {(agents.length > 0 || run.guardian_id) && <div className="autopilot-agent-list">
		{run.guardian_id && <div key={run.guardian_id}><span className="muted">guardian</span> <strong>{run.guardian_id}</strong> <span>{['stopped','complete'].includes(run.state) ? 'done' : 'idle'}</span></div>}
		{agents.map((a) => <div key={a.id}><span className="muted">{a.id === run.brain?.agent_id ? 'brain' : 'worker'}</span> <strong>{a.name || a.id}</strong> <span>{a.status}</span></div>)}
	  </div>}

      {run.backoff && (
        <div className="autopilot-run-backoff warn">
          backoff stage {run.backoff.stage} · retry {relativeTime(run.backoff.next_retry_at)}
          {run.backoff.last_error && <span> · {run.backoff.last_error}</span>}
        </div>
      )}
    </section>
  );
}

// relativeTime formats an ISO timestamp as a short "Xs ago" string.
function relativeTime(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime();
  if (isNaN(ms) || ms < 0) return iso;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  return `${h}h ago`;
}
