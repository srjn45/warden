import { useState, useRef } from 'react';
import { quickAdd } from '../lib/quickadd';

// QuickAddButton is the per-pane '+' that spawns a no-prompt agent in `dir`.
// It owns its own busy / confirm / error state so AgentGrid stays presentational.
// A memory-pressure 428 flips it into a force state; the next click forces.
export default function QuickAddButton({ dir, onCreated }: {
  dir: string;
  onCreated: (id: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [confirmReason, setConfirmReason] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const pending = useRef(false);

  async function click() {
    if (pending.current) return;
    pending.current = true;
    setBusy(true);
    setError(null);
    const res = await quickAdd(dir, confirmReason !== null);
    setBusy(false);
    pending.current = false;
    if (res.kind === 'created') {
      setConfirmReason(null);
      onCreated(res.id);
    } else if (res.kind === 'confirm') {
      setConfirmReason(res.reason);
    } else {
      setError(res.message);
    }
  }

  const warn = confirmReason !== null || error !== null;
  const title = error
    ? `spawn failed: ${error}`
    : confirmReason
      ? `⚠ memory pressure: ${confirmReason} — click again to spawn anyway`
      : `Launch a new agent in ${dir}`;

  return (
    <button
      type="button"
      className={`grid-group-add${warn ? ' warn' : ''}`}
      disabled={busy}
      title={title}
      aria-label={busy ? 'Spawning agent…' : `Add agent in ${dir}`}
      onClick={(e) => { e.stopPropagation(); click(); }}
    >
      {busy ? '…' : '+'}
    </button>
  );
}
