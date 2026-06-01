import { useState } from 'react';
import { cleanup, ApiError } from '../lib/api';

export default function TerminateControls({ id, onDone }: { id: string; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [guard, setGuard] = useState<string | null>(null);
  const [hard, setHard] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  async function run(force: boolean) {
    setBusy(true);
    setErr(null);
    try {
      await cleanup(id, force, hard);
      onDone();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setGuard(e.message); // uncommitted/unpushed guard — offer force
      } else {
        setErr(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setBusy(false);
    }
  }

  if (guard) {
    return (
      <div className="terminate guard">
        <p className="warn">{guard}</p>
        <label>
          <input type="checkbox" checked={hard} onChange={(e) => setHard(e.target.checked)} />{' '}
          also hard-delete the record
        </label>
        <div className="actions">
          <button className="danger" disabled={busy} onClick={() => run(true)}>
            Force terminate (remove worktree + branch)
          </button>
          <button disabled={busy} onClick={() => setGuard(null)}>Cancel</button>
        </div>
      </div>
    );
  }

  return (
    <div className="terminate">
      <button className="danger" disabled={busy} onClick={() => run(false)}>Terminate</button>
      {err && <span className="warn"> {err}</span>}
    </div>
  );
}
