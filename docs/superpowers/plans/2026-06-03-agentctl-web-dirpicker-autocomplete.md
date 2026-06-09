# Web DirPicker Autocomplete Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the web directory picker usable for directories with many children — an editable path field with live, prefix-filtered autocomplete over a fixed-height scrollable subdirectory list, plus ↑/↓/Enter keyboard navigation.

**Architecture:** Frontend-only. Pure parsing/filtering helpers (`splitPath`, `filterEntries`) live in a new unit-tested `web/src/lib/dirpath.ts`; `DirPicker.tsx` is rewritten to consume them and the existing `listDirs(path)` API (no daemon change). A directory is always represented in the input as `<path>/` (trailing slash) so its trailing segment is treated as the current dir, not a filter; typing without a trailing slash filters the parent's children.

**Tech Stack:** Astro + React 19 + TypeScript, Vitest (jsdom). Existing `web/src/lib/api.ts` exports `DirEntry { name; path }`, `DirListing { path; parent; entries }`, and `listDirs(path?): Promise<DirListing>`.

**Testing approach note:** This repo unit-tests pure lib functions only (no `@testing-library`, no component-render tests). `splitPath`/`filterEntries` are fully unit-tested; the `DirPicker` component is verified via `tsc --noEmit` + `npm run build`.

---

## File Structure

- **Create** `web/src/lib/dirpath.ts` — pure `splitPath(query)` + `filterEntries(entries, leaf)`.
- **Test** `web/src/lib/dirpath.test.ts` — unit tests for both.
- **Rewrite** `web/src/components/DirPicker.tsx` — editable path input + filtered scrollable list + keyboard nav. Same props `{ value: string | null; onChange: (path: string) => void }` (consumers `NewAgentModal`, `QuickSpawn` unchanged).
- **Modify** `web/src/styles/app.css` — append `.dirpicker-input`, `.dirpicker-list` (max-height + scroll), row + highlight styles (no existing `.dirpicker` rules today).

---

## Task 1: Pure helpers `dirpath.ts`

**Files:**
- Create: `web/src/lib/dirpath.ts`
- Test: `web/src/lib/dirpath.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/dirpath.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { splitPath, filterEntries } from './dirpath';
import type { DirEntry } from './api';

describe('splitPath', () => {
  it('trailing slash → baseDir is the dir, empty leaf', () => {
    expect(splitPath('/a/b/')).toEqual({ baseDir: '/a/b', leaf: '' });
  });
  it('no trailing slash → dirname + leaf', () => {
    expect(splitPath('/a/b')).toEqual({ baseDir: '/a', leaf: 'b' });
  });
  it('root variants', () => {
    expect(splitPath('/')).toEqual({ baseDir: '/', leaf: '' });
    expect(splitPath('/x')).toEqual({ baseDir: '/', leaf: 'x' });
  });
  it('no slash → empty baseDir (backend home), whole string is the leaf', () => {
    expect(splitPath('foo')).toEqual({ baseDir: '', leaf: 'foo' });
  });
  it('empty → both empty', () => {
    expect(splitPath('')).toEqual({ baseDir: '', leaf: '' });
  });
});

describe('filterEntries', () => {
  const entries: DirEntry[] = [
    { name: 'workspace', path: '/u/workspace' },
    { name: 'Work-notes', path: '/u/Work-notes' },
    { name: 'Documents', path: '/u/Documents' },
  ];
  it('empty leaf returns all entries', () => {
    expect(filterEntries(entries, '')).toHaveLength(3);
  });
  it('prefix match, case-insensitive', () => {
    expect(filterEntries(entries, 'wor').map((e) => e.name)).toEqual(['workspace', 'Work-notes']);
  });
  it('no match returns empty', () => {
    expect(filterEntries(entries, 'zzz')).toEqual([]);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/dirpath.test.ts`
Expected: FAIL — cannot find module `./dirpath`.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/dirpath.ts`:

```ts
import type { DirEntry } from './api';

// splitPath divides a typed path into the directory whose children should be
// listed (baseDir) and the trailing segment used to filter them (leaf).
// A trailing slash means "I'm inside this directory" (no filter); otherwise the
// last segment filters the parent's children. An empty baseDir means the backend
// default (home).
export function splitPath(query: string): { baseDir: string; leaf: string } {
  if (query.endsWith('/')) {
    const trimmed = query.slice(0, -1);
    return { baseDir: trimmed === '' ? '/' : trimmed, leaf: '' };
  }
  const i = query.lastIndexOf('/');
  if (i < 0) return { baseDir: '', leaf: query };
  const baseDir = query.slice(0, i);
  return { baseDir: baseDir === '' ? '/' : baseDir, leaf: query.slice(i + 1) };
}

// filterEntries keeps the subdirectories whose name starts with leaf
// (case-insensitive). An empty leaf returns every entry.
export function filterEntries(entries: DirEntry[], leaf: string): DirEntry[] {
  if (leaf === '') return entries;
  const p = leaf.toLowerCase();
  return entries.filter((e) => e.name.toLowerCase().startsWith(p));
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/dirpath.test.ts`
Expected: PASS (8 assertions across both describes).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/dirpath.ts web/src/lib/dirpath.test.ts
git commit -m "feat(web): splitPath + filterEntries helpers for DirPicker autocomplete"
```

---

## Task 2: Rewrite `DirPicker.tsx`

**Files:**
- Rewrite: `web/src/components/DirPicker.tsx`

- [ ] **Step 1: Replace the component**

Replace the ENTIRE contents of `web/src/components/DirPicker.tsx` with:

```tsx
import { useEffect, useState } from 'react';
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

  // load fetches a directory's children. On failure it keeps the last good
  // listing (so a half-typed segment doesn't blank the list) and shows a hint.
  async function load(path?: string): Promise<DirListing | null> {
    try {
      const l = await listDirs(path);
      setDir(l.path);
      setEntries(l.entries);
      setParent(l.parent);
      setErr(null);
      return l;
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      return null;
    }
  }

  // Initial load (backend home); seed the input with "<home>/".
  useEffect(() => {
    void load().then((l) => { if (l) setQuery(l.path + '/'); });
  }, []);

  const { baseDir, leaf } = splitPath(query);

  // When the typed baseDir differs from the loaded dir, (debounced) load it.
  useEffect(() => {
    if (dir === null) return;        // initial load still in flight
    if (baseDir === dir) return;     // already showing this directory's children
    const t = setTimeout(() => { void load(baseDir || undefined); }, 150);
    return () => clearTimeout(t);
  }, [baseDir, dir]);

  const visible = filterEntries(entries, leaf);
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
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors. (`NewAgentModal`/`QuickSpawn` still type-check — props unchanged.)

- [ ] **Step 3: Verify the build**

Run: `cd web && npm run build`
Expected: build succeeds.

- [ ] **Step 4: Commit**

```bash
git add web/src/components/DirPicker.tsx
git commit -m "feat(web): DirPicker editable path + filtered scrollable list + keyboard nav"
```

---

## Task 3: Styles + final verification

**Files:**
- Modify: `web/src/styles/app.css` (append)

- [ ] **Step 1: Append the styles**

Append the following to the END of `web/src/styles/app.css` (there are no existing
`.dirpicker` rules — these are purely additive; do not modify other rules):

```css
/* ── Directory picker ── */
.dirpicker { display: flex; flex-direction: column; gap: .4rem; }
.dirpicker-input { width: 100%; padding: .4rem; font: inherit; box-sizing: border-box; }
.dirpicker-list { max-height: 220px; overflow-y: auto; list-style: none; margin: 0; padding: 0; border: 1px solid #8883; border-radius: .3rem; }
.dirpicker-list li button { width: 100%; text-align: left; background: none; border: none; padding: .3rem .5rem; cursor: pointer; color: inherit; font: inherit; }
.dirpicker-list li button:hover, .dirpicker-list li button.on { background: #2f81f733; }
.dirpicker-empty { padding: .3rem .5rem; }
.dirpicker-use { align-self: flex-start; }
```

- [ ] **Step 2: Verify the build**

Run: `cd web && npm run build`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add web/src/styles/app.css
git commit -m "style(web): fixed-height scrollable DirPicker list + highlight"
```

- [ ] **Step 4: Full verification**

Run: `cd web && npx tsc --noEmit && npm run build && npx vitest run`
Expected: tsc clean; build succeeds; ALL web tests pass (the existing suite plus the new
`dirpath` tests).

- [ ] **Step 5: Manual smoke (the part automated tests don't cover)**

Rebuild + restart the daemon and open the web UI's "New agent" modal (and the Overview
quick-spawn). Verify:
- The subdirectory list is height-capped and scrolls when a directory has many children.
- Typing into the path field filters the list (case-insensitive prefix); typing `<dir>/`
  descends into it; clearing back filters the parent.
- ↑/↓ move the highlight, Enter descends into the highlighted row (and `../`).
- "Use this folder" commits the loaded directory (shows `✓`), and spawning from it works.

---

## Self-Review (completed by plan author)

**Spec coverage:**
- Fixed-height scrollable list → Task 3 (`.dirpicker-list { max-height; overflow-y:auto }`). ✓
- Editable path field + live prefix autocomplete → Task 2 (input + `splitPath`/`filterEntries`). ✓
- Keyboard ↑/↓/Enter → Task 2 (`onKeyDown`, `highlight`, `activate`). ✓
- `../` only when not filtering → Task 2 (`showUp = leaf === '' && parent !== ''`). ✓
- "Use this folder" commits loaded `dir`, not raw text → Task 2 (`onClick={() => dir && onChange(dir)}`). ✓
- baseDir always real (descend appends `/`) → Task 2 (`descend` + initial `setQuery(l.path + '/')`). ✓
- Failure keeps last good listing → Task 2 (`load` catch sets err only, leaves entries). ✓
- Pure helpers unit-tested; component build-verified → Tasks 1, 2, 3. ✓
- No backend change → confirmed (only `web/` touched). ✓

**Placeholder scan:** No TBD/TODO/vague steps; every code step shows full code. ✓

**Type consistency:** `splitPath` returns `{ baseDir, leaf }` and `filterEntries(entries, leaf)` — used identically in `DirPicker.tsx`. `DirEntry`/`DirListing`/`listDirs` match `web/src/lib/api.ts`. Props `{ value, onChange }` unchanged from the current component, so consumers are unaffected. ✓
