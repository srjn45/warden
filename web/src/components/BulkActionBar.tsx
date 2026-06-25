import { useState } from 'react';
import { bulkTerminate, bulkDelete, bulkMessage, summarize, type BatchResult } from '../lib/batch';

// BulkActionBar is the floating action bar shown while one or more agents are
// selected in the Cockpit grid (#21 batch operations). It fans the chosen ids
// out over the existing per-agent endpoints (sequential — #36 parallel backing
// is parked) and reports partial success. Destructive actions require a second
// click to confirm.
export default function BulkActionBar({ selected, onClear }: {
  selected: string[];
  onClear: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<'terminate' | 'delete' | null>(null);
  const [composing, setComposing] = useState(false);
  const [msg, setMsg] = useState('');

  const n = selected.length;

  async function run(op: () => Promise<BatchResult[]>) {
    setBusy(true); setStatus(null);
    try {
      const results = await op();
      setStatus(summarize(results));
      // Clear only when every agent succeeded; otherwise keep the selection so
      // the operator can see/retry the failures.
      if (results.every((r) => r.ok)) onClear();
    } finally {
      setBusy(false);
      setConfirm(null);
    }
  }

  async function doSend() {
    const body = msg.trim();
    if (!body) return;
    await run(() => bulkMessage(selected, body));
    setMsg('');
    setComposing(false);
  }

  return (
    <div className="bulk-bar">
      <span className="bulk-count">{n} selected</span>
      {composing ? (
        <span className="bulk-compose">
          <input
            type="text"
            placeholder="message…"
            value={msg}
            disabled={busy}
            onChange={(e) => setMsg(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') doSend(); }}
            autoFocus
          />
          <button disabled={busy || !msg.trim()} onClick={doSend}>Send</button>
          <button disabled={busy} onClick={() => { setComposing(false); setMsg(''); }}>Cancel</button>
        </span>
      ) : (
        <button disabled={busy} onClick={() => { setComposing(true); setStatus(null); }}>Message…</button>
      )}

      {confirm === 'terminate' ? (
        <button className="danger" disabled={busy} onClick={() => run(() => bulkTerminate(selected))}>
          Confirm terminate {n}
        </button>
      ) : (
        <button className="danger" disabled={busy || composing} onClick={() => setConfirm('terminate')}>
          Terminate
        </button>
      )}

      {confirm === 'delete' ? (
        <button className="danger" disabled={busy} onClick={() => run(() => bulkDelete(selected))}>
          Confirm delete {n}
        </button>
      ) : (
        <button className="danger" disabled={busy || composing} onClick={() => setConfirm('delete')}>
          Delete
        </button>
      )}

      <button disabled={busy} onClick={onClear}>Clear</button>
      {status && <span className="bulk-status">{status}</span>}
    </div>
  );
}
