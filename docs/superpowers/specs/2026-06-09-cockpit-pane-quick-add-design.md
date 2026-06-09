# Cockpit pane redesign + per-pane quick-add

**Date:** 2026-06-09
**Status:** Approved (design)

## Problem

The web Cockpit tab renders agents grouped by directory (`AgentGrid.tsx`), but
the grouping is visually weak:

- The group header is a single muted line — `{dir} ({count})` — small (`.8rem`)
  and low-contrast. The full path dominates and the folder identity is hard to
  scan.
- Groups have no boundary (`.agent-grid-group { margin-bottom: 1rem }`); adjacent
  groups blur together, so it doesn't read as a "pane".
- To launch another agent in the same directory you must open the New-agent
  modal and re-pick that directory by hand. There is no fast path.

## Goals

1. Make each directory group read as a distinct, titled **pane**.
2. Make the header clearer: prominent folder name, de-emphasized full path,
   visible agent count.
3. Add a per-pane **quick-add** (`+`) that spawns a new agent in that pane's
   directory with no prompt, in one click.

## Non-goals

- No change to the tile contents (`MiniTerminal`, `BusyIdleBadge`).
- No supervised toggle on quick-add — it is unsupervised (warden default),
  hardcoded.
- No quick-add on the Overview mini-grid (Overview already has Quick spawn).

## Design

### 1. Titled pane + header bar — `AgentGrid.tsx` + `app.css`

Each directory group becomes a bordered, rounded pane (consistent with
`.overview .card`). The group renders a tinted header **bar** across the top,
then the existing tile grid in the body below.

Header bar contents:
- **Left:** bold folder basename + dimmed full path beside it. The path
  truncates with an ellipsis when it overflows so the bar never wraps.
- **Right:** agent count, then the `+` quick-add button (see §2).

Markup shape (within `AgentGrid`'s `groups.map`):

```tsx
<div key={g.dir} className="agent-grid-group">
  <div className="grid-group-bar">
    <span className="grid-group-name">{baseName(g.dir)}</span>
    <span className="grid-group-path">{g.dir}</span>
    <span className="grid-group-count">{g.sessions.length}</span>
    {onCreated && g.dir !== '—' && (
      <QuickAddButton dir={g.dir} onCreated={onCreated} />
    )}
  </div>
  <div className="agent-grid"> … existing tiles … </div>
</div>
```

New / changed CSS (`app.css`, "Agent grid / cockpit" block):
- `.agent-grid-group` — add `border: 1px solid #8884; border-radius: .5rem;
  overflow: hidden;` (keep bottom margin).
- `.grid-group-bar` — `display: flex; align-items: center; gap: .5rem;
  padding: .4rem .6rem; background: #8881; border-bottom: 1px solid #8884;`
- `.grid-group-name` — `font-weight: 600;`
- `.grid-group-path` — `color: var(--idle); font-size: .8rem; overflow: hidden;
  text-overflow: ellipsis; white-space: nowrap; min-width: 0; flex: 1;` (the
  flex:1 + min-width:0 lets it absorb slack and truncate, pushing count/button
  to the right).
- `.grid-group-count` — `color: var(--idle); font-size: .8rem;`
- `.grid-group-add` — small square button: `border: 1px solid #8884;
  border-radius: .3rem; background: transparent; color: inherit; cursor:
  pointer; line-height: 1; padding: .1rem .4rem;` hover → `border-color:
  #2f81f7`. A `.grid-group-add.warn` variant tints it when in force/error state.
- The old `.grid-group-head` rule is removed (class no longer used).
- `.agent-grid` gains `padding: .6rem` so the tiles sit inset from the pane
  border rather than touching it.

### 2. `baseName(dir)` — pure helper in `group.ts`

```ts
// baseName returns the last path segment of a grouping dir for the pane title.
// Trailing slashes are ignored. The sentinel '—' (unknown dir) is returned
// as-is. An empty result falls back to the original dir.
export function baseName(dir: string): string { … }
```

Behavior (unit-tested in `group.test.ts`):
- `/Users/x/workspace/personal/warden` → `warden`
- `/Users/x/warden/` (trailing slash) → `warden`
- `—` → `—`
- `warden` (no slash) → `warden`
- `/` → `/` (fallback to original when last segment empty)

### 3. Quick-add: pure helper + thin button

The codebase tests **pure logic in `lib/`** (there are no React component
tests). So the spawn side-effect is split:

**`lib/quickadd.ts`** — pure, unit-tested helper:

```ts
export type QuickAddResult =
  | { kind: 'created'; id: string }
  | { kind: 'confirm'; reason: string }  // 428 memory pressure — needs force
  | { kind: 'error'; message: string };

// quickAdd does a no-prompt, unsupervised spawn in `dir`. force re-tries past a
// memory-pressure 428. Never throws — maps every outcome to a QuickAddResult.
export async function quickAdd(dir: string, force?: boolean): Promise<QuickAddResult>;
```

**`QuickAddButton.tsx`** — thin presentational shell (no test, matching the
codebase convention). Props `{ dir: string; onCreated: (id: string) => void }`.
Owns busy / confirm / error state and renders the `+`:
1. Click → `quickAdd(dir)`.
2. `created` → `onCreated(id)` (Dashboard opens that agent's tab).
3. `confirm` → store the reason; button gets a force affordance and `title`
   = `⚠ memory pressure: <reason> — click again to spawn anyway`. A second click
   calls `quickAdd(dir, true)`.
4. `error` → `title` = message, button gets `.warn` tint.

While in flight the button is `disabled`. Label is `+` normally; warn-tinted in
the force/error state — the `title` carries the explanation.

### 4. Wiring — thread `onCreated`

- `AgentGrid` gains an optional prop `onCreated?: (id: string) => void`.
- `CockpitTab` gains `onCreated` and passes it through to `AgentGrid`.
- `Dashboard` passes `onCreated={(id) => dispatch({ kind: 'open', id })}` to
  `CockpitTab` (same effect the modal's `onCreated` has, minus closing a modal).
- `OverviewTab` does **not** pass `onCreated` to its mini-grid → no `+` there.

## Data flow

```
QuickAddButton click
  → spawn({ prompt:'', cwd: dir, supervised:false[, force] })   (api.ts)
  → POST /spawn
  → 200 Session  → onCreated(id) → Dashboard dispatch open → new agent tab
  or 428 verdict → ConfirmationRequiredError → force state → retry w/ force
```

## Error handling / edge cases

- **Unknown-dir group (`—`)**: no real cwd, so the `+` is not rendered.
- **Memory pressure (428)**: surfaced + retryable, never silently dropped.
- **Spawn error**: surfaced via the button `title` + warn tint; pane otherwise
  unaffected.
- **Overview nesting**: Overview's mini-grid is inside a `.card`; it gains the
  pane border (card → pane → tile). Accepted for visual consistency.

## Testing

- **Unit (`group.test.ts`)**: `baseName()` cases above.
- **Unit (`quickadd.test.ts`)**: `quickAdd()` stubs `fetch` (same pattern as
  `api.test.ts`) and asserts the spawn body (`prompt:''`, correct `cwd`,
  `supervised:false`, `force`), and the result mapping for 200 → `created`,
  428 → `confirm`, 5xx → `error`.
- **Manual / Playwright**: cockpit renders titled panes with header bars;
  clicking `+` opens a new agent tab in the right directory; `—` group has no
  `+`.

## Files touched

- `web/src/lib/group.ts` — add `baseName()`.
- `web/src/lib/group.test.ts` — `baseName()` tests.
- `web/src/lib/quickadd.ts` — new pure helper.
- `web/src/lib/quickadd.test.ts` — new.
- `web/src/components/QuickAddButton.tsx` — new (thin shell, untested).
- `web/src/components/AgentGrid.tsx` — pane/header markup, optional `onCreated`.
- `web/src/components/CockpitTab.tsx` — thread `onCreated`.
- `web/src/components/Dashboard.tsx` — pass `onCreated` to `CockpitTab`.
- `web/src/styles/app.css` — pane + header bar + quick-add styling.
