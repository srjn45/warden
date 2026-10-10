import { useCallback, useEffect, useState } from 'react';
import { ApiError, getFastBrainMetrics, listFastBrainDecisions, setFastBrainControl } from '../lib/api';
import {
  classLabel, fmtMs, outcomeSegments, splitByTier,
  type FBControl, type FBDecision, type FBKindTier, type FBTelemetry,
} from '../lib/fastbrain';

// FastBrainCard shows the Fast-Brain (cheap, latency-sensitive) and
// Thinking-Brain (strategic) decision metrics side by side: outcome bars,
// latency percentiles, queue depth, runner/circuit health, per-kind pause
// controls and a recent-decisions trace. Everything shown is bounded
// vocabulary (kind/tier/outcome/provider/model) — never prompts or IDs.
export default function FastBrainCard() {
  const [tel, setTel] = useState<FBTelemetry | null>(null);
  const [decisions, setDecisions] = useState<FBDecision[]>([]);
  const [err, setErr] = useState<ApiError | null>(null);

  const refresh = useCallback(() => {
    getFastBrainMetrics().then((t) => { setTel(t); setErr(null); })
      .catch((e) => { if (e instanceof ApiError) setErr(e); });
    listFastBrainDecisions(25).then(setDecisions).catch(() => {});
  }, []);

  useEffect(() => {
    refresh();
    const h = setInterval(refresh, 5000);
    return () => clearInterval(h);
  }, [refresh]);

  const toggle = (c: FBControl) => {
    setFastBrainControl(c.kind, !c.paused).then(refresh).catch((e) => { if (e instanceof ApiError) setErr(e); });
  };

  if (!tel) {
    return (
      <section className="card metrics-card metrics-wide">
        <h3>Fast / Thinking brain</h3>
        <p className="muted">{err ? `Unavailable: ${err.message}` : 'Loading…'}</p>
      </section>
    );
  }
  const { fast, thinking } = splitByTier(tel.kinds);
  const paused = tel.controls.filter((c) => c.paused);
  return (
    <section className="card metrics-card metrics-wide fb-card">
      <h3>Fast / Thinking brain</h3>
      <div className="fb-summary">
        <span>admitted <b>{tel.admitted}</b></span>
        <span>active <b>{tel.active}/{tel.max_concurrent}</b></span>
        <span>cache hits <b>{tel.cache_hits}</b></span>
        <span>coalesced <b>{tel.coalesced}</b></span>
        <span>preempted <b>{tel.preempted}</b></span>
        <span>abandoned <b>{tel.abandoned_live}</b>/{tel.abandoned_total}</span>
        {tel.queues.map((q) => (
          <span key={q.class}>{classLabel(q.class)} queue <b>{q.queued}</b></span>
        ))}
      </div>
      {err && <p className="muted">Last refresh failed: {err.message}</p>}
      <div className="fb-tiers">
        <TierTable title="Fast brain" rows={fast} />
        <TierTable title="Thinking brain" rows={thinking} />
      </div>
      <h4>Runners &amp; circuits</h4>
      <table className="fb-table">
        <thead><tr><th>provider</th><th>model</th><th>tier</th><th>calls</th><th>ok</th><th>failed</th></tr></thead>
        <tbody>
          {tel.runners.map((r) => (
            <tr key={`${r.provider}/${r.model ?? ''}/${r.tier}`}>
              <td>{r.provider}</td><td>{r.model ?? '—'}</td><td>{r.tier}</td>
              <td>{r.calls}</td><td>{r.ok}</td><td>{r.failed}</td>
            </tr>
          ))}
          {tel.runners.length === 0 && <tr><td colSpan={6} className="muted">no runner calls yet</td></tr>}
        </tbody>
      </table>
      <div className="fb-circuits">
        {tel.circuits.map((c) => (
          <span key={c.provider} className={`fb-badge fb-${c.state}`} title={c.last_failure ?? ''}>
            {c.provider}: {c.state}{c.consecutive_failures ? ` (${c.consecutive_failures} fails)` : ''}
          </span>
        ))}
      </div>
      <h4>Controls {paused.length > 0 && <span className="fb-badge fb-open">{paused.length} paused</span>}</h4>
      <div className="fb-controls">
        {tel.controls.map((c) => (
          <button key={c.kind} className={c.paused ? 'fb-paused' : ''} onClick={() => toggle(c)}
            title={c.paused ? `paused (${c.source ?? 'operator'}) — click to resume` : 'click to pause (fails open)'}>
            {c.kind}: {c.paused ? 'resume' : 'pause'}
          </button>
        ))}
      </div>
      <h4>Recent decisions</h4>
      <table className="fb-table">
        <thead><tr><th>time</th><th>kind</th><th>tier</th><th>outcome</th><th>action</th><th>provider</th><th>wait</th><th>run</th></tr></thead>
        <tbody>
          {decisions.map((d, i) => (
            <tr key={`${d.time}-${i}`}>
              <td>{new Date(d.time).toLocaleTimeString()}</td><td>{d.kind}</td><td>{d.tier}</td>
              <td>{d.outcome}</td><td>{d.final_action}</td><td>{d.provider ?? '—'}</td>
              <td>{fmtMs(d.queue_wait_ms)}</td><td>{fmtMs(d.run_ms)}</td>
            </tr>
          ))}
          {decisions.length === 0 && <tr><td colSpan={8} className="muted">no decisions recorded</td></tr>}
        </tbody>
      </table>
    </section>
  );
}

function TierTable({ title, rows }: { title: string; rows: FBKindTier[] }) {
  return (
    <div className="fb-tier">
      <h4>{title}</h4>
      {rows.length === 0 ? <p className="muted">no activity</p> : rows.map((k) => (
        <div key={`${k.kind}/${k.tier}`} className="fb-kind">
          <div className="fb-kind-head">
            <span>{k.kind}</span>
            <span className="muted">{classLabel(k.class)} · {k.attempts} attempts · p50 {fmtMs(k.run_time.p50_ms)} · p95 {fmtMs(k.run_time.p95_ms)} · wait p95 {fmtMs(k.queue_wait.p95_ms)}</span>
          </div>
          <div className="fb-bar">
            {outcomeSegments(k.outcomes).map((s) => (
              <span key={s.name} className={`fb-seg fb-o-${s.name}`} style={{ width: `${s.frac * 100}%` }}
                title={`${s.name}: ${s.n}`} />
            ))}
          </div>
          <div className="muted fb-kind-foot">
            {Object.entries(k.outcomes).map(([n, v]) => `${n} ${v}`).join(' · ')}
            {k.cached ? ` · cached ${k.cached}` : ''}{k.coalesced ? ` · coalesced ${k.coalesced}` : ''}
            {k.fail_open ? ` · fail-open ${k.fail_open}` : ''}
          </div>
        </div>
      ))}
    </div>
  );
}
