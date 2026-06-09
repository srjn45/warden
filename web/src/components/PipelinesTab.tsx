import { useEffect, useState } from 'react';
import type { Pipeline, PipelineJob } from '../lib/types';
import { listPipelines, cancelPipeline, deletePipeline, retryJob, startPipeline, ApiError } from '../lib/api';
import { jobStatusClass, isJobRetryable, pipelineHasLiveJobs, pipelineIsCancelable, jobDigestSummary } from '../lib/pipelines';
import PipelineDag from './PipelineDag';
import NewPipelineModal from './NewPipelineModal';

// PipelinesTab polls /pipelines while mounted (the SSE channel carries sessions,
// not pipelines). Jobs are sessions, so "Open terminal" reuses onSelect to pin
// the agent tab. Each pipeline renders as a layered DAG (PipelineDag); authoring
// is available here too via NewPipelineModal (start/cancel/retry/delete inline).
export default function PipelinesTab({ onSelect }: { onSelect: (id: string) => void }) {
  const [pipelines, setPipelines] = useState<Pipeline[]>([]);
  const [selId, setSelId] = useState<string | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);
  const [authoring, setAuthoring] = useState(false);

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

  const onDelete = (id: string) => {
    if (!window.confirm(`Delete pipeline "${id}"? This removes its record permanently.`)) return;
    deletePipeline(id)
      .then(() => { setSelId(null); setJobId(null); return listPipelines().then(setPipelines); })
      .catch((e) => {
        const msg = e instanceof ApiError ? e.message : 'delete failed';
        window.alert(`Could not delete ${id}: ${msg}`);
      });
  };

  const onCreated = (id: string) => {
    setAuthoring(false);
    setSelId(id);
    setJobId(null);
    listPipelines().then(setPipelines).catch(() => { /* next poll */ });
  };

  return (
    <div className="pipelines">
      <aside className="pipe-list">
        <button className="btn pipe-new" onClick={() => setAuthoring(true)}>+ New pipeline</button>
        {pipelines.length === 0 && (
          <div className="empty">No pipelines yet. Create one above, or with <code>warden pipeline create -f spec.yaml</code>.</div>
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
            {selected.status === 'pending' && (
              <button className="btn" onClick={() => startPipeline(selected.id).catch(() => { /* ignore */ })}>Start</button>
            )}
            <button
              className="btn"
              disabled={!pipelineIsCancelable(selected)}
              title={pipelineIsCancelable(selected) ? '' : 'Pipeline already finished — delete it instead'}
              onClick={() => cancelPipeline(selected.id).catch(() => { /* ignore */ })}
            >Cancel</button>
            {!pipelineHasLiveJobs(selected) && (
              <button className="btn" onClick={() => onDelete(selected.id)}>Delete</button>
            )}
          </header>
          <div className="dag-scroll">
            <PipelineDag jobs={selected.jobs} selected={jobId} onSelect={setJobId} />
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
          {drawerJob.branch && (<><label>Branch</label><pre className="job-text">{drawerJob.branch}</pre></>)}
          {jobDigestSummary(drawerJob) && (<><label>Digest</label><pre className="job-text">{jobDigestSummary(drawerJob)}</pre></>)}
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

      {authoring && (
        <NewPipelineModal onClose={() => setAuthoring(false)} onCreated={onCreated} />
      )}
    </div>
  );
}
