import { useState } from 'react';
import type { Session } from '../lib/types';
import { terminate, removeWorktree, deleteSession, ApiError } from '../lib/api';

// TerminateControls drives the real teardown endpoints:
//   Terminate         -> POST /sessions/{id}/terminate         (stop the agent)
//   Remove worktree   -> POST /sessions/{id}/remove-worktree   (force on 409 guard)
//   Hard-delete record-> POST /sessions/{id}/delete {hard:true}
export default function TerminateControls({ session, onDone }: {
  session: Session;
  onDone: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [guard, setGuard] = useState<string | null>(null); // 409 message from remove-worktree

  async function doTerminate() {
    setBusy(true); setErr(null);
    try {
      await terminate(session.id);
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  }

  async function doRemoveWorktree(force: boolean) {
    setBusy(true); setErr(null);
    try {
      await removeWorktree(session.id, force);
      setGuard(null);
      onDone();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setGuard(e.message);
      else setErr(e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  }

  async function doDelete() {
    setBusy(true); setErr(null);
    try {
      await deleteSession(session.id, true);
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  }

  if (guard) {
    return (
      <div className="terminate guard">
        <p className="warn">{guard}</p>
        <div className="actions">
          <button className="danger" disabled={busy} onClick={() => doRemoveWorktree(true)}>
            Force remove worktree + branch
          </button>
          <button disabled={busy} onClick={() => setGuard(null)}>Cancel</button>
        </div>
      </div>
    );
  }

  return (
    <div className="terminate">
      <div className="actions">
        <button className="danger" disabled={busy} onClick={doTerminate}>Terminate</button>
        {session.worktree && (
          <button disabled={busy} onClick={() => doRemoveWorktree(false)}>Remove worktree</button>
        )}
        <button disabled={busy} onClick={doDelete}>Delete record</button>
      </div>
      {err && <span className="warn"> {err}</span>}
    </div>
  );
}
