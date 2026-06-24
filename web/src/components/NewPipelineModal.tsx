import { useMemo, useState } from 'react';
import { createPipeline, ApiError } from '../lib/api';
import {
  emptyDraft, emptyJob, validateDraft, draftToSpec, specToYaml,
  type PipelineDraft, type JobDraft,
} from '../lib/pipeline-spec';

// NewPipelineModal is a structured pipeline authoring form: pipeline name + repo,
// then one card per job (id, prompt, depends_on, worktree mode, supervised,
// handoff). The draft is validated client-side with the same rules as the daemon
// (lib/pipeline-spec mirrors pipeline.Validate) for instant feedback, then
// serialized to JSON (a valid YAML subset) and POSTed to the existing
// POST /pipelines. On success the caller gets the new pipeline id (so it can
// offer Start).
export default function NewPipelineModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [draft, setDraft] = useState<PipelineDraft>(emptyDraft());
  const [serverErr, setServerErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const errors = useMemo(() => validateDraft(draft), [draft]);
  const jobIds = draft.jobs.map((j) => j.id).filter((id) => id.trim() !== '');

  function patchJob(i: number, patch: Partial<JobDraft>) {
    setDraft((d) => ({ ...d, jobs: d.jobs.map((j, k) => (k === i ? { ...j, ...patch } : j)) }));
  }
  function addJob() {
    setDraft((d) => ({ ...d, jobs: [...d.jobs, emptyJob()] }));
  }
  function removeJob(i: number) {
    setDraft((d) => ({ ...d, jobs: d.jobs.filter((_, k) => k !== i) }));
  }

  async function submit() {
    setServerErr(null);
    if (errors.length > 0) return;
    setBusy(true);
    try {
      const p = await createPipeline(specToYaml(draftToSpec(draft)));
      onCreated(p.id);
    } catch (e) {
      setServerErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal pipe-modal" onClick={(e) => e.stopPropagation()}>
        <h2>New pipeline</h2>

        <div className="pipe-modal-head">
          <label>Name
            <input
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              placeholder="my-pipeline"
              autoFocus
            />
          </label>
          <label>Repo
            <input
              value={draft.repo}
              onChange={(e) => setDraft({ ...draft, repo: e.target.value })}
              placeholder="/path/to/repo"
            />
          </label>
        </div>

        <div className="pipe-jobs">
          {draft.jobs.map((j, i) => (
            <div className="pipe-job-card" key={i}>
              <div className="pipe-job-row">
                <label className="grow">Job id
                  <input
                    value={j.id}
                    onChange={(e) => patchJob(i, { id: e.target.value })}
                    placeholder={`job-${i + 1}`}
                  />
                </label>
                {draft.jobs.length > 1 && (
                  <button type="button" className="pipe-job-rm" title="Remove job" onClick={() => removeJob(i)}>✕</button>
                )}
              </div>
              <label>Prompt
                <textarea
                  rows={3}
                  value={j.prompt}
                  onChange={(e) => patchJob(i, { prompt: e.target.value })}
                  placeholder="What should this job do?"
                />
              </label>
              <div className="pipe-job-row">
                <label className="grow">Depends on
                  <select
                    multiple
                    value={j.depends_on}
                    onChange={(e) => patchJob(i, {
                      depends_on: Array.from(e.target.selectedOptions, (o) => o.value),
                    })}
                  >
                    {jobIds.filter((id) => id !== j.id).map((id) => (
                      <option key={id} value={id}>{id}</option>
                    ))}
                  </select>
                </label>
                <label className="grow">Worktree
                  <select value={j.worktree} onChange={(e) => patchJob(i, { worktree: e.target.value })}>
                    <option value="none">none</option>
                    <option value="fresh">fresh</option>
                    {jobIds.filter((id) => id !== j.id).map((id) => (
                      <option key={id} value={`from:${id}`}>from:{id}</option>
                    ))}
                  </select>
                </label>
                <label className="grow">Run if
                  <select value={j.run_if} onChange={(e) => patchJob(i, { run_if: e.target.value as JobDraft['run_if'] })}>
                    <option value="success">success (deps ok)</option>
                    <option value="failure">failure (a dep failed)</option>
                    <option value="always">always</option>
                  </select>
                </label>
              </div>
              <label className="supervised-toggle">
                <input
                  type="checkbox"
                  checked={j.supervised}
                  onChange={(e) => patchJob(i, { supervised: (e.target as HTMLInputElement).checked })}
                />
                Supervised (prompts for risky tools — answer in the inbox)
              </label>
              <label>Handoff hint <span className="muted">(optional)</span>
                <input
                  value={j.handoff}
                  onChange={(e) => patchJob(i, { handoff: e.target.value })}
                  placeholder="what to tell downstream jobs"
                />
              </label>
            </div>
          ))}
          <button type="button" className="pipe-add-job" onClick={addJob}>+ Add job</button>
        </div>

        {errors.length > 0 && (
          <ul className="pipe-errors">
            {errors.map((e, i) => <li key={i} className="warn">{e}</li>)}
          </ul>
        )}
        {serverErr && <p className="warn">{serverErr}</p>}

        <div className="actions">
          <button disabled={busy || errors.length > 0} onClick={submit}>Create pipeline</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
