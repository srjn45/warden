import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';

const TYPES = ['development', 'analysis', 'spike', 'pr-review', 'buildkite-debug', 'test-run', 'env-test', 'other'];

export default function NewAgentModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [type, setType] = useState('development');
  const [ticket, setTicket] = useState('');
  const [repo, setRepo] = useState('');
  const [branch, setBranch] = useState('');
  const [pr, setPr] = useState('');
  const [worktree, setWorktree] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const showBranch = type === 'development' || type === 'pr-review';
  const showPr = type === 'pr-review';
  const showWorktree = type === 'analysis' || type === 'spike';

  async function submit() {
    setErr(null);
    if (!repo.trim()) { setErr('repo is required'); return; }
    if (type === 'pr-review' && !pr.trim() && !branch.trim()) {
      setErr('pr-review needs a PR number or a branch'); return;
    }
    setBusy(true);
    try {
      const s = await spawn({ type, ticket, repo, branch, pr, worktree });
      onCreated(s.id);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>New agent</h2>
        <label>Type
          <select value={type} onChange={(e) => setType(e.target.value)}>
            {TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        </label>
        <label>Repo path
          <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="/Users/…/the-monorepo" />
        </label>
        <label>Ticket (optional)
          <input value={ticket} onChange={(e) => setTicket(e.target.value)} placeholder="PROJ-350" />
        </label>
        {showBranch && (
          <label>Branch {type === 'pr-review' ? '(checkout target)' : '(new)'}
            <input value={branch} onChange={(e) => setBranch(e.target.value)} />
          </label>
        )}
        {showPr && (
          <label>PR number/url
            <input value={pr} onChange={(e) => setPr(e.target.value)} />
          </label>
        )}
        {showWorktree && (
          <label>
            <input type="checkbox" checked={worktree} onChange={(e) => setWorktree(e.target.checked)} />{' '}
            create scratch worktree
          </label>
        )}
        {err && <p className="warn">{err}</p>}
        <div className="actions">
          <button disabled={busy} onClick={submit}>Create</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
