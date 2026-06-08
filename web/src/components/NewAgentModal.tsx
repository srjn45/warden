import { useState } from 'react';
import { spawn, ApiError, ConfirmationRequiredError } from '../lib/api';
import DirPicker from './DirPicker';

export default function NewAgentModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [prompt, setPrompt] = useState('');
  const [dir, setDir] = useState<string | null>(null);
  const [supervised, setSupervised] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [confirm, setConfirm] = useState<string | null>(null);

  async function doSpawn(force: boolean) {
    setErr(null);
    if (!dir) { setErr('choose a directory to launch the agent from'); return; }
    setBusy(true);
    try {
      const s = await spawn({ prompt, cwd: dir, supervised, force });
      onCreated(s.id);
    } catch (e) {
      if (e instanceof ConfirmationRequiredError) {
        setConfirm(e.verdict.reason);
      } else {
        setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h2>New agent</h2>
        <label>What should this agent do? <span className="muted">(leave blank to open Claude and type instructions yourself)</span>
          <textarea
            rows={6}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Describe a task to run autonomously, or leave blank…"
            autoFocus
            onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doSpawn(false); }}
          />
        </label>
        <label>Launch directory
          <DirPicker value={dir} onChange={setDir} />
        </label>
        <p className="muted">The type label is assigned automatically once it starts.</p>
        <label className="supervised-toggle">
          <input type="checkbox" checked={supervised} onChange={(e) => setSupervised((e.target as HTMLInputElement).checked)} />
          Supervised (prompts for risky tools — answer in the inbox)
        </label>
        {err && <p className="warn">{err}</p>}
        {confirm && (
          <p className="warn">⚠ memory pressure: {confirm}. Spawn anyway?</p>
        )}
        <div className="actions">
          {confirm
            ? <button disabled={busy} onClick={() => doSpawn(true)}>Spawn anyway</button>
            : <button disabled={busy || !dir} onClick={() => doSpawn(false)}>Launch agent</button>}
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
