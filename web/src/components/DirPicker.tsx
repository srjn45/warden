import { useEffect, useState } from 'react';
import { listDirs, type DirListing } from '../lib/api';

export default function DirPicker({ value, onChange }: {
  value: string | null;
  onChange: (path: string) => void;
}) {
  const [listing, setListing] = useState<DirListing | null>(null);
  const [err, setErr] = useState<string | null>(null);

  async function load(path?: string) {
    setErr(null);
    try {
      setListing(await listDirs(path));
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }

  useEffect(() => { void load(); }, []);

  return (
    <div className="dirpicker">
      <div className="dirpicker-path muted">{listing?.path ?? 'loading…'}</div>
      {err && <p className="warn">{err}</p>}
      <ul className="dirpicker-list">
        {listing?.parent && (
          <li><button type="button" onClick={() => void load(listing.parent)}>../</button></li>
        )}
        {listing?.entries.map((e) => (
          <li key={e.path}><button type="button" onClick={() => void load(e.path)}>{e.name}/</button></li>
        ))}
      </ul>
      <button
        type="button"
        className="dirpicker-use"
        disabled={!listing}
        onClick={() => listing && onChange(listing.path)}
      >
        Use this folder{listing && value === listing.path ? ' ✓' : ''}
      </button>
      {value && <p className="muted">Selected: {value}</p>}
    </div>
  );
}
