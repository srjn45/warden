import { useEffect, useState } from 'react';
import type { Pipeline, PipelineJob } from '../lib/types';
import { listPipelines, cancelPipeline, retryJob } from '../lib/api';
import { jobStatusClass, isJobRetryable } from '../lib/pipelines';

// PipelinesTab polls /pipelines while mounted (the SSE channel carries sessions,
// not pipelines). Jobs are sessions, so "Open terminal" reuses onSelect to pin
// the agent tab. Read-only view + cancel/retry; authoring is via the CLI.
export default function PipelinesTab({ onSelect }: { onSelect: (id: string) => void }) {
  const [pipelines, setPipelines] = useState<Pipeline[]>([]);
  const [selId, setSelId] = useState<string | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);

  useEffect(() => {
    let on = true;
    const load = () => listPipelines().then((ps) => { if (on) setPipelines(ps); }).catch(() => { /* keep last */ });
    load();
    const t = setInterval(load, 2000);
    return () => { on = false; clearInterval(t); };
  }, []);

  const selected = pipelines.find((p) => p.id === selId) ?? pipelines[0] ?? null;
  const drawerJob: PipelineJob | null = selected && jobId
    ? selected.jobs.find((j) => j.id === jobId) ?? null
    : null;

  return (
    <div className="pipelines">
      <aside className="pipe-list">
        {pipelines.length === 0 && (
          <div className="empty">No pipelines yet. Create one with <code>agentctl pipeline create -f spec.yaml</code>.</div>
        )}
        {pipelines.map((p) => (
          <button
            key={p.id}
            className={`pipe-item${p.id === selected?.id ? ' on' : ''}`}
            onClick={() => { setSelId(p.id); setJobId(null); }}
          >
            <span className="pipe-name">{p.id}</span>
            <span className={`pipe-status st-${p.status}`}>{p.status}</span>
          </button>
        ))}
      </aside>

      {selected && (
        <section className="pipe-detail">
          <header className="pipe-head">
            <h2>{selected.id} <span className={`pipe-status st-${selected.status}`}>{selected.status}</span></h2>
            <button className="btn" onClick={() => cancelPipeline(selected.id).catch(() => { /* ignore */ })}>Cancel</button>
          </header>
          <div className="job-grid">
            {selected.jobs.map((j) => (
              <button
                key={j.id}
                className={`job-card ${jobStatusClass(j.status)}${j.id === jobId ? ' on' : ''}`}
                onClick={() => setJobId(j.id)}
              >
                <div className="job-id">{j.id}</div>
                <div className="job-st">{j.status}</div>
                {j.depends_on && j.depends_on.length > 0 && (
                  <div className="job-deps">← {j.depends_on.join(', ')}</div>
                )}
              </button>
            ))}
          </div>
        </section>
      )}

      {drawerJob && selected && (
        <aside className="job-drawer">
          <header className="drawer-head">
            <h3>{drawerJob.id} <span className={jobStatusClass(drawerJob.status)}>{drawerJob.status}</span></h3>
            <button className="tab-close" title="Close" onClick={() => setJobId(null)}>✕</button>
          </header>
          <label>Prompt</label>
          <pre className="job-text">{drawerJob.prompt}</pre>
          {drawerJob.handoff && (<><label>Handoff hint</label><pre className="job-text">{drawerJob.handoff}</pre></>)}
          {drawerJob.output && (<><label>Output</label><pre className="job-text">{drawerJob.output}</pre></>)}
          <div className="drawer-actions">
            {drawerJob.session_id && drawerJob.status === 'running' && (
              <button className="btn" onClick={() => onSelect(drawerJob.session_id)}>Open terminal</button>
            )}
            {isJobRetryable(drawerJob.status) && (
              <button className="btn" onClick={() => retryJob(selected.id, drawerJob.id).catch(() => { /* ignore */ })}>Retry</button>
            )}
          </div>
        </aside>
      )}
    </div>
  );
}
