# Web DirPicker — scrollable, type-to-autocomplete

**Date:** 2026-06-03
**Status:** Approved (brainstorm) — ready for implementation plan
**Scope:** Web interface only (`web/`). The TUI (`internal/tui/`) is NOT touched.

## Problem

The web directory picker (`web/src/components/DirPicker.tsx`, used by the "New agent"
modal and the Overview quick-spawn) is click-to-browse only. When a directory has a large
number of subdirectories the rendered list grows unbounded — there is no height cap and no
way to type a path or filter. The user must click through every level.

## Goal

Make the web DirPicker usable for directories with many children:

1. **Fixed-height, scrollable** subdirectory list (no more unbounded growth).
2. **Editable path text field** with **live autocomplete**: as the user types a path, the
   list filters to matching subdirectories.
3. **Keyboard navigation** of the suggestions (↑/↓/Enter), in addition to clicking.

This is a **frontend-only** change. The existing `GET /fs/dirs` endpoint already returns a
directory's full child list, parent, and resolved path, so filtering and the height cap are
done client-side. No daemon/Go changes.

## Interaction model (unified path field + filtered list)

```
┌───────────────────────────────────────┐
│ /Users/me/workspace/per▌               │  ← editable path <input>
├───────────────────────────────────────┤
│ ../                                    │  ← shown only when not filtering
│ ▸ personal/                 (match)    │  ┐ fixed height
│   periph-tools/             (match)    │  │ overflow-y: auto
│   …filtered by what you type           │  ┘
├───────────────────────────────────────┤
│ [ Use this folder ]   Selected: …      │
└───────────────────────────────────────┘
```

- The input holds a free-text `query`. The component tracks a loaded directory `dir` (the
  directory whose children are currently in `entries`), its `parent`, an error/hint, and a
  `highlight` index.
- On each `query` change, split into `(baseDir, leaf)`:
  - If `query` ends with `/`: `baseDir` = `query` without the trailing slash (or `/` at
    root), `leaf` = `""`.
  - Otherwise: `baseDir` = the portion before the last `/` (i.e. the dirname; `/` when that
    portion is empty, as in `/x`), `leaf` = the trailing segment after the last `/`.
  - A `query` with no `/` at all is treated as `baseDir = ""` (→ backend defaults to home),
    `leaf = query`.
- When the derived `baseDir` differs from the currently loaded `dir`, **debounce ~150ms**
  then call `listDirs(baseDir)`. On success, set `dir`, `entries`, `parent`, clear error,
  reset `highlight` to 0. On failure (not a dir / unreadable — e.g. a half-typed segment
  that isn't itself a directory), keep the last good `entries` visible and show a subtle
  inline hint; do not clear the list.
- The visible list = `entries` filtered by `leaf` (case-insensitive **prefix** match on the
  entry name). A `../` row is prepended **only when `leaf === ""`** (pure browse); while
  filtering it is omitted as noise. `../` is omitted when `parent === ""` (filesystem root).
- **Why `baseDir` is always a real directory:** typing `/Users/me/workspa` yields
  `baseDir = /Users/me`, `leaf = workspa` → lists `/Users/me`, filters to `workspace`.
  `baseDir` only advances to `/Users/me/workspace` once the user types the next `/`.

### Selecting / navigating

- **Descend into an entry** (click a row, or Enter on the highlighted row): set `query` to
  `entry.path + "/"`. This changes `baseDir`, which auto-loads that entry's children.
- **`../`** (click or Enter when highlighted): set `query` to `parent + "/"`.
- **Keyboard** (when the input is focused): ↑/↓ move `highlight` within the visible rows
  (the `../` row, when present, is index 0); Enter activates the highlighted row (descend or
  go up). Highlight clamps to the visible row range and resets to 0 whenever the visible
  list changes.
- **"Use this folder"** button: calls `onChange(dir)` — it commits the **loaded directory**
  `dir`, never the raw typed text, so the committed value is guaranteed to be a real
  directory. Disabled until a directory has loaded. Shows `✓` when `value === dir`, and a
  `Selected: <value>` line when `value` is set (preserving current behavior).

### Initial state

On mount, `listDirs()` (no path → backend home) loads; `query` is initialized to the
returned `dir` path.

## Decomposition

Following the repo convention (pure logic is unit-tested; React components are
build-verified, no `@testing-library`):

**New — `web/src/lib/dirpath.ts`** (pure, unit-tested):
- `splitPath(query: string): { baseDir: string; leaf: string }` — the parsing rule above.
- `filterEntries(entries: DirEntry[], leaf: string): DirEntry[]` — case-insensitive prefix
  match on `name`; returns all entries when `leaf === ""`.

(`DirEntry` is the existing type exported from `web/src/lib/api.ts`.)

**Rewrite — `web/src/components/DirPicker.tsx`**: consumes `splitPath`/`filterEntries` and
`listDirs`. Same props (`{ value: string | null; onChange: (path: string) => void }`) — so
`NewAgentModal` and `QuickSpawn` are unaffected.

**Modify — `web/src/styles/app.css`**: cap `.dirpicker-list` height with `overflow-y: auto`,
and add a highlighted-row style.

## Testing

- **Vitest unit (`web/src/lib/dirpath.test.ts`)**:
  - `splitPath`: trailing slash (`/a/b/` → `{/a/b, ""}`), no trailing slash
    (`/a/b` → `{/a, b}`), root (`/` → `{/, ""}` ; `/x` → `{/, x}`), no slash
    (`foo` → `{"", foo}`), empty (`""` → `{"", ""}`).
  - `filterEntries`: prefix match, case-insensitivity, empty leaf returns all, no-match
    returns empty.
- **Component**: verified via `npx tsc --noEmit` + `npm run build` (no render tests, per
  convention).
- Full web suite (`npx vitest run`) stays green.

## Out of scope

- Backend `/fs/dirs` changes (server-side filtering, fuzzy match) — client-side prefix
  filtering on the already-returned child list is sufficient.
- Showing hidden (dot) directories — the endpoint already excludes them; unchanged.
- Substring/fuzzy matching — prefix only.
- Any change to the TUI directory picker / `/fs/dirs` tab-completion.
