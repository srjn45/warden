import { useEffect, useState } from 'react';
import type { Session } from '../lib/types';
import { createTerminal, terminate, ApiError } from '../lib/api';
import { terminalName } from '../lib/kind';
import AttachTerminal from './AttachTerminal';
import DirPicker from './DirPicker';
import BusyIdleBadge from './BusyIdleBadge';

// TerminalsTab is the web parity for the TUI's Terminals section + terminal
// viewport (spec §10). It lists the live kind=terminal sessions on the left and
// attaches the selected one's real PTY on the right — the same AttachTerminal
// engine the per-agent view uses, since a terminal is an ordinary session with
// its own tmux. "New terminal" spawns a shell in a chosen directory; the ✕
// terminates one. Selection follows the live list: a freshly created terminal is
// auto-selected once it appears over SSE, and closing the selected one hands off
// to whatever remains.
export default function TerminalsTab({ terminals }: { terminals: Session[] }) {
  const [selected, setSelected] = useState<string | null>(null);
  const [showNew, setShowNew] = useState(false);

  // Keep the selection valid as the live list changes: default to the first
  // terminal, and when the selected one disappears fall back to the first.
  useEffect(() => {
    if (terminals.length === 0) {
      if (selected !== null) setSelected(null);
      return;
    }
    if (selected === null || !terminals.some((t) => t.id === selected)) {
      setSelected(terminals[0].id);
    }
  }, [terminals, selected]);

  function close(id: string) {
    // Fire-and-forget: the list reconciles over SSE when the session ends.
    terminate(id).catch(() => { /* already gone / will reconcile */ });
  }

  return (
    <div className="terminals">
      <aside className="terminals-list card">
        <div className="terminals-head">
          <h3>Terminals</h3>
          <button onClick={() => setShowNew(true)}>+ New terminal</button>
        </div>
        {terminals.length === 0
          ? <p className="muted">No terminals open. Open one to get a plain shell.</p>
          : (
            <ul>
              {terminals.map((t) => (
                <li key={t.id} className={`terminals-row${t.id === selected ? ' on' : ''}`}>
                  <button className="terminals-pick" onClick={() => setSelected(t.id)}>
                    <span className="terminals-name">{terminalName(t)}</span>
                    <BusyIdleBadge status={t.status} exitCode={t.exit_code} />
                  </button>
                  <button className="terminals-close" title="Close terminal" onClick={() => close(t.id)}>✕</button>
                </li>
              ))}
            </ul>
          )}
      </aside>
      <section className="terminals-view">
        {selected
          ? <AttachTerminal key={selected} id={selected} />
          : <div className="detail empty">No terminal selected.</div>}
      </section>
      {showNew && (
        <NewTerminalModal
          onClose={() => setShowNew(false)}
          onCreated={(id) => { setShowNew(false); setSelected(id); }}
        />
      )}
    </div>
  );
}

// NewTerminalModal picks a launch directory and opens a plain shell there. It is
// the terminal analogue of NewAgentModal but far simpler — no prompt, role, or
// backend, because the daemon ignores all of those for kind=terminal.
function NewTerminalModal({ onClose, onCreated }: {
  onClose: () => void;
  onCreated: (id: string) => void;
}) {
  const [dir, setDir] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function create() {
    setErr(null);
    if (!dir) { setErr('choose a directory to open the terminal in'); return; }
    setBusy(true);
    try {
      const s = await createTerminal(dir);
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
        <h2>New terminal</h2>
        <label>Directory
          <DirPicker value={dir} onChange={setDir} />
        </label>
        <p className="muted">Opens a plain shell in this directory — no AI agent.</p>
        {err && <p className="warn">{err}</p>}
        <div className="actions">
          <button disabled={busy || !dir} onClick={create}>Open terminal</button>
          <button onClick={onClose}>Cancel</button>
        </div>
      </div>
    </div>
  );
}
