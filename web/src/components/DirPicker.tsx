import { useCallback, useEffect, useRef, useState } from 'react';
import { listDirs, type DirEntry, type DirListing } from '../lib/api';
import { splitPath, filterEntries } from '../lib/dirpath';

// DirPicker lets the user pick a launch directory by typing a path (with live
// prefix autocomplete) or browsing. A loaded directory is represented in the
// input as "<path>/" so its trailing segment is the current dir, not a filter.
export default function DirPicker({ value, onChange }: {
  value: string | null;
  onChange: (path: string) => void;
}) {
  const [query, setQuery] = useState('');
  const [dir, setDir] = useState<string | null>(null); // currently loaded directory
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [parent, setParent] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [highlight, setHighlight] = useState(0);

  const reqId = useRef(0);

  // load fetches a directory's children. A monotonic request id guards against
  // out-of-order responses (fast typing): only the latest load commits state.
  // On failure it keeps the last good listing (so a half-typed segment doesn't
  // blank the list) and shows a hint.
  const load = useCallback(async (path?: string): Promise<DirListing | null> => {
    const id = ++reqId.current;
    try {
      const l = await listDirs(path);
      if (id !== reqId.current) return null; // superseded by a newer load
      setDir(l.path);
      setEntries(l.entries);
      setParent(l.parent);
      setErr(null);
      return l;
    } catch (e) {
      if (id !== reqId.current) return null;
      setErr(e instanceof Error ? e.message : String(e));
      return null;
    }
  }, []);

  // Initial load (backend home); seed the input with "<home>/".
  useEffect(() => {
    void load().then((l) => { if (l) setQuery(l.path + '/'); });
  }, [load]);

  const { baseDir, leaf } = splitPath(query);

  // When the typed baseDir differs from the loaded dir, (debounced) load it.
  useEffect(() => {
    if (dir === null) return;        // initial load still in flight
    if (baseDir === dir) return;     // already showing this directory's children
    const t = setTimeout(() => { void load(baseDir || undefined); }, 150);
    return () => clearTimeout(t);
  }, [baseDir, dir, load]);

  const visible = filterEntries(entries, leaf);
  // `../` is offered only while browsing (no active filter); a non-empty leaf is
  // a filter, and `..` wouldn't match it. Clear the trailing segment to go up.
  const showUp = leaf === '' && parent !== '';

  type Row = { key: string; label: string; target: string };
  const rows: Row[] = [];
  if (showUp) rows.push({ key: '__up__', label: '../', target: parent });
  for (const e of visible) rows.push({ key: e.path, label: `${e.name}/`, target: e.path });

  // Reset the highlight whenever the visible list changes.
  useEffect(() => { setHighlight(0); }, [dir, leaf]);

  function descend(path: string) {
    setQuery(path.endsWith('/') ? path : path + '/');
  }
  function activate(index: number) {
    const r = rows[index];
    if (r) descend(r.target);
  }
  function onKeyDown(ev: React.KeyboardEvent) {
    if (ev.key === 'ArrowDown') { ev.preventDefault(); setHighlight((h) => Math.min(h + 1, rows.length - 1)); }
    else if (ev.key === 'ArrowUp') { ev.preventDefault(); setHighlight((h) => Math.max(h - 1, 0)); }
    else if (ev.key === 'Enter') { ev.preventDefault(); activate(highlight); }
  }

  return (
    <div className="dirpicker">
      <input
        className="dirpicker-input"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={onKeyDown}
        placeholder="Type or browse a directory…"
        spellCheck={false}
      />
      {err && <p className="warn">{err}</p>}
      <ul className="dirpicker-list">
        {rows.length === 0 && <li className="muted dirpicker-empty">no matching subdirectories</li>}
        {rows.map((r, i) => (
          <li key={r.key}>
            <button
              type="button"
              className={i === highlight ? 'on' : ''}
              onClick={() => descend(r.target)}
            >{r.label}</button>
          </li>
        ))}
      </ul>
      <button
        type="button"
        className="dirpicker-use"
        disabled={dir === null}
        onClick={() => dir && onChange(dir)}
      >
        Use this folder{dir && value === dir ? ' ✓' : ''}
      </button>
      {value && <p className="muted">Selected: {value}</p>}
    </div>
  );
}
