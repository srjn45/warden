import { useCallback, useEffect, useState } from 'react';
import {
  listBackends, rescanBackends, setDefaultBackend, setThinkingMode, patchBackend,
  ApiError, type Backend, type BackendsState,
} from '../lib/api';
import BackendLogo from './BackendLogo';

// The user-assignable billing tiers, in ladder order. The reserved `local` tier
// is system-set and never offered in the dropdown.
const TIERS = ['free', 'subscription', 'pay_per_use', 'unclassified'] as const;
const TIER_LABEL: Record<string, string> = {
  free: 'Free',
  subscription: 'Subscription',
  pay_per_use: 'Pay per use',
  unclassified: 'Unclassified',
  local: 'Local',
};

// BackendsPanel is the agent-backend registry settings surface (spec §9): a
// table of detected backends with a per-row tier dropdown, a single-choice
// default radio, and an enable toggle, plus a thinking-mode selector and a
// Rescan button. Opens from the "🧩 backends" button in the AttentionBar and
// mirrors the TUI Backends page. Every mutation goes through the Stage-2 daemon
// endpoints and re-lists so the whole table stays coherent.
export default function BackendsPanel({ onClose }: { onClose: () => void }) {
  const [state, setState] = useState<BackendsState | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A local clock that advances every second so the limited-until countdowns
  // tick smoothly without a refetch on every frame.
  const [, setNow] = useState(Date.now());

  const load = useCallback(() => {
    listBackends()
      .then((s) => { setState(s); setError(null); })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);

  useEffect(() => { load(); }, [load]);

  // Advance the countdown clock every second; refetch on a slower cadence so
  // externally-changed rows (a fresh rate-limit, another client's edit) surface
  // without disrupting an open dropdown every tick.
  useEffect(() => {
    const clock = setInterval(() => setNow(Date.now()), 1000);
    const poll = setInterval(load, 5000);
    return () => { clearInterval(clock); clearInterval(poll); };
  }, [load]);

  // Run a mutation, surface any error, then re-list so the whole table reflects
  // the write (the single-row / settings-only responses are intentionally
  // dropped in favor of a full, coherent reload).
  const mutate = useCallback(async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await fn();
      setState(await listBackends());
    } catch (e) {
      setError(e instanceof ApiError ? e.message : e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  }, []);

  const backends = state?.backends ?? [];
  const mode = state?.settings.internal_thinking_mode ?? 'local_only';

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal backends-panel" onClick={(e) => e.stopPropagation()}>
        <header className="autopilot-panel-head">
          <h2>Backends</h2>
          <button className="context-drawer-close" title="Close" onClick={onClose}>✕</button>
        </header>

        {/* Header controls: internal-thinking mode selector + rescan. */}
        <div className="backends-controls">
          <label className="backends-mode">
            <span>Internal thinking</span>
            <select
              value={mode}
              disabled={busy || state == null}
              onChange={(e) => mutate(() => setThinkingMode(e.target.value))}
              aria-label="Internal thinking mode"
            >
              <option value="local_only">Local only</option>
              <option value="free_plus_local">Free + local</option>
            </select>
          </label>
          <button
            className="backends-rescan"
            disabled={busy}
            onClick={() => mutate(() => rescanBackends())}
            title="Re-detect installed CLIs on PATH"
          >
            ⟳ Rescan
          </button>
        </div>

        {error && <p className="warn backends-error">{error}</p>}

        {state == null && !error && <p className="muted backends-empty">Loading…</p>}

        {state != null && backends.length === 0 && (
          <p className="muted backends-empty">No backends detected. Install a CLI, then Rescan.</p>
        )}

        {backends.length > 0 && (
          <div className="backends-table-wrap">
            <table className="backends-table">
              <thead>
                <tr>
                  <th scope="col">Backend</th>
                  <th scope="col">Installed</th>
                  <th scope="col">Tier</th>
                  <th scope="col">Default</th>
                  <th scope="col">Enabled</th>
                  <th scope="col">Limited</th>
                </tr>
              </thead>
              <tbody>
                {backends.map((b) => (
                  <BackendRow key={b.id} b={b} busy={busy} mutate={mutate} />
                ))}
              </tbody>
            </table>
          </div>
        )}

        <p className="muted backends-foot">
          The <strong>local</strong> row is the reserved $0 model — its tier is system-set
          and it can never be the default. Internal-thinking mode routes warden's own
          (non-user-facing) thinking.
        </p>
      </div>
    </div>
  );
}

function BackendRow({
  b, busy, mutate,
}: {
  b: Backend;
  busy: boolean;
  mutate: (fn: () => Promise<unknown>) => void;
}) {
  // The daemon only accepts a default that is a real, installed, enabled,
  // non-local backend — mirror that so the radio is inert (not error-prone) for
  // the rows it would reject.
  const canDefault = !b.is_local && b.installed && b.enabled;
  return (
    <tr className={b.is_local ? 'backends-row local' : 'backends-row'}>
      <td className="backends-id">
        <BackendLogo backend={b.id} /> <span>{b.id}</span>
      </td>
      <td className="backends-center">{b.installed ? '✓' : '—'}</td>
      <td>
        {b.is_local ? (
          <span className="muted">{TIER_LABEL.local}</span>
        ) : (
          <select
            value={b.tier}
            disabled={busy}
            onChange={(e) => mutate(() => patchBackend(b.id, { tier: e.target.value }))}
            aria-label={`Tier for ${b.id}`}
          >
            {TIERS.map((t) => (
              <option key={t} value={t}>{TIER_LABEL[t]}</option>
            ))}
            {/* Keep an unexpected server-side tier visible rather than blank. */}
            {!(TIERS as readonly string[]).includes(b.tier) && (
              <option value={b.tier}>{b.tier}</option>
            )}
          </select>
        )}
      </td>
      <td className="backends-center">
        <input
          type="radio"
          name="backends-default"
          checked={b.default}
          disabled={busy || !canDefault}
          onChange={() => mutate(() => setDefaultBackend(b.id))}
          aria-label={`Set ${b.id} as the default backend`}
          title={canDefault ? 'Set as the default backend' : 'This backend cannot be the default'}
        />
      </td>
      <td className="backends-center">
        <input
          type="checkbox"
          checked={b.enabled}
          disabled={busy}
          onChange={(e) => mutate(() => patchBackend(b.id, { enabled: e.target.checked }))}
          aria-label={`Enable ${b.id}`}
        />
      </td>
      <td className="backends-center backends-limited">{limitedLabel(b)}</td>
    </tr>
  );
}

// limitedLabel renders the remaining rate-limit window as a short "Xm Ys"
// countdown, or an em-dash when the backend is available.
export function limitedLabel(b: Backend): string {
  if (!b.limited_until) return '—';
  const ms = new Date(b.limited_until).getTime() - Date.now();
  if (isNaN(ms) || ms <= 0) return '—';
  const s = Math.ceil(ms / 1000);
  const m = Math.floor(s / 60);
  const r = s % 60;
  return m > 0 ? `${m}m ${r}s` : `${r}s`;
}
