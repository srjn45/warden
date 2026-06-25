import { useEffect, useMemo, useState } from 'react';
import type { Session } from '../lib/types';
import { getHistory } from '../lib/api';
import { filterSessions } from '../lib/search';

// Task types offered in the Archive filter; mirrors the daemon's known types.
const TYPES = ['development', 'pr-review', 'analysis', 'spike', 'code', 'docs', 'website', 'debug-ci', 'tests', 'other'];

// SINCE_OPTIONS map a label to a millisecond look-back ('' = all time).
const SINCE_OPTIONS: { label: string; ms: number }[] = [
  { label: 'all time', ms: 0 },
  { label: 'last 24h', ms: 24 * 3600 * 1000 },
  { label: 'last 7d', ms: 7 * 24 * 3600 * 1000 },
  { label: 'last 30d', ms: 30 * 24 * 3600 * 1000 },
];

// ArchiveTab browses the closed/ store (#29): the records the soft-delete path
// persists. Server-side since/type filters narrow the fetch; a text box filters
// the result client-side with the same matcher the Overview search bar uses.
export default function ArchiveTab() {
  const [rows, setRows] = useState<Session[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [type, setType] = useState('');
  const [sinceIdx, setSinceIdx] = useState(0);
  const [query, setQuery] = useState('');

  useEffect(() => {
    let cancelled = false;
    setLoading(true); setErr(null);
    const ms = SINCE_OPTIONS[sinceIdx].ms;
    const sinceISO = ms > 0 ? new Date(Date.now() - ms).toISOString() : undefined;
    getHistory({ sinceISO, type: type || undefined })
      .then((r) => { if (!cancelled) setRows(r); })
      .catch((e) => { if (!cancelled) setErr(e instanceof Error ? e.message : String(e)); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [type, sinceIdx]);

  const shown = useMemo(() => filterSessions(rows, query), [rows, query]);

  return (
    <div className="archive">
      <div className="archive-filters">
        <h3>Archive</h3>
        <select value={sinceIdx} onChange={(e) => setSinceIdx(Number(e.target.value))} aria-label="Since">
          {SINCE_OPTIONS.map((o, i) => <option key={o.label} value={i}>{o.label}</option>)}
        </select>
        <select value={type} onChange={(e) => setType(e.target.value)} aria-label="Type">
          <option value="">all types</option>
          {TYPES.map((t) => <option key={t} value={t}>{t}</option>)}
        </select>
        <input
          type="search"
          placeholder="search archive…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          aria-label="Search archive"
        />
        <span className="muted">{shown.length} archived</span>
      </div>

      {err && <p className="warn">{err}</p>}
      {loading ? (
        <p className="muted">Loading…</p>
      ) : shown.length === 0 ? (
        <p className="muted">No archived agents match.</p>
      ) : (
        <table className="archive-table">
          <thead>
            <tr>
              <th>ID</th><th>Name</th><th>Type</th><th>Status</th>
              <th>Branch</th><th>Updated</th><th>Subject</th>
            </tr>
          </thead>
          <tbody>
            {shown.map((s) => (
              <tr key={s.id}>
                <td>{s.id}</td>
                <td>{s.name || '—'}</td>
                <td>{s.type || '—'}</td>
                <td>{s.status}</td>
                <td>{s.branch || '—'}</td>
                <td>{s.updated_at ? new Date(s.updated_at).toLocaleString() : '—'}</td>
                <td>{s.subject}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
