import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';
import DirPicker from './DirPicker';

// QuickSpawn is the inline New-agent form on the Overview tab: a prompt plus a
// directory picker. Mirrors NewAgentModal's logic without the modal chrome.
export default function QuickSpawn({ onCreated }: { onCreated: (id: string) => void }) {
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
      setPrompt('');
      onCreated(s.id);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="quick-spawn">
      <textarea
        rows={3}
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        placeholder="What should a new agent do? (⌘/Ctrl+Enter to launch)"
        onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit(); }}
      />
      <DirPicker value={dir} onChange={setDir} />
        <label className="supervised-toggle">
          <input type="checkbox" checked={supervised} onChange={(e) => setSupervised((e.target as HTMLInputElement).checked)} />
          Supervised (prompts for risky tools — answer in the inbox)
        </label>
      {err && <p className="warn">{err}</p>}
      <button disabled={busy || !dir} onClick={submit}>Launch agent</button>
    </div>
  );
}
