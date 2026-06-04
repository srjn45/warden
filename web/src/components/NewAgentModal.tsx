import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';
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

  async function submit() {
    setErr(null);
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
    if (!dir) { setErr('choose a directory to launch the agent from'); return; }
    setBusy(true);
    try {
      const s = await spawn({ prompt, cwd: dir, supervised });
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
        <label>What should this agent do?
          <textarea
            rows={6}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="e.g. Review the auth module for security issues and propose fixes…"
            autoFocus
            onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit(); }}
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
        <div className="actions">
          <button disabled={busy || !dir} onClick={submit}>Create</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
