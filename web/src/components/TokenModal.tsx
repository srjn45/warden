import { useState } from 'react';
import { setToken } from '../lib/token';

// TokenModal blocks the UI when the daemon requires a bearer token the browser
// doesn't have (or has a stale one). Saving stores the token and lets the
// Dashboard retry; there is no cancel — without a valid token nothing loads.
export default function TokenModal({ onSaved }: { onSaved: () => void }) {
  const [value, setValue] = useState('');

  function save(e: React.FormEvent) {
    e.preventDefault();
    const t = value.trim();
    if (!t) return;
    setToken(t);
    onSaved();
  }

  return (
    <div className="modal-backdrop">
      <form className="modal token-modal" onSubmit={save} role="dialog" aria-modal="true" aria-label="Access token required">
        <h2>Access token required</h2>
        <p className="muted">
          This warden daemon requires an access token. Paste the value printed by{' '}
          <code>warden token generate</code> (your <code>WARDEN_TOKEN</code>).
        </p>
        <label>Token
          <input
            type="password"
            autoFocus
            autoComplete="off"
            placeholder="Bearer token"
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
        </label>
        <div className="actions">
          <button type="submit" disabled={!value.trim()}>Save &amp; continue</button>
        </div>
      </form>
    </div>
  );
}
