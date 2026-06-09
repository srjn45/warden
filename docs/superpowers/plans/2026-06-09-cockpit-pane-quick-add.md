# Cockpit Pane Redesign + Per-Pane Quick-Add Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make each directory group on the web Cockpit tab read as a distinct titled pane (bold folder name + dim path + count in a header bar) and add a per-pane `+` button that spawns a no-prompt agent in that directory.

**Architecture:** `AgentGrid` (shared by Cockpit and Overview) renders each `groupSessions()` bucket as a bordered pane with a header bar. A new pure helper `baseName()` (in `group.ts`) supplies the folder title. The quick-add spawn side-effect lives in a pure, unit-tested `lib/quickadd.ts`; a thin `QuickAddButton.tsx` shell drives it and holds button UI state. The `+` is wired only when an `onCreated` callback is threaded down (`Dashboard → CockpitTab → AgentGrid`), so Overview's mini-grid stays `+`-free.

**Tech Stack:** Astro + React 19, TypeScript, Vitest (jsdom) for pure-logic tests. The web app is served by the Go daemon from the embedded `web/dist` build.

**Spec:** `docs/superpowers/specs/2026-06-09-cockpit-pane-quick-add-design.md`

**Conventions to honor:**
- Tests are pure-logic only, in `web/src/lib/*.test.ts`. There are **no** React component tests and no `@testing-library/react`. Do not add component tests or new test deps.
- `fetch` is stubbed in tests via `vi.stubGlobal('fetch', …)` (see `web/src/lib/api.test.ts`).
- Run all commands from `web/`. Test command: `npm test` (alias for `vitest run`). Single file: `npx vitest run src/lib/<file>.test.ts`.
- Commit messages end with the `Co-Authored-By` trailer used in this repo.

---

## Task 1: `baseName()` pure helper

**Files:**
- Modify: `web/src/lib/group.ts`
- Test: `web/src/lib/group.test.ts`

- [ ] **Step 1: Write the failing tests**

Append to `web/src/lib/group.test.ts` (and add `baseName` to the existing import on line 2 so it reads `import { groupSessions, sourceDir, baseName } from './group';`):

```ts
describe('baseName', () => {
  it('returns the last path segment', () => {
    expect(baseName('/Users/x/workspace/personal/warden')).toBe('warden');
  });
  it('ignores a trailing slash', () => {
    expect(baseName('/Users/x/warden/')).toBe('warden');
  });
  it('returns the dash sentinel as-is', () => {
    expect(baseName('—')).toBe('—');
  });
  it('returns a bare name unchanged', () => {
    expect(baseName('warden')).toBe('warden');
  });
  it('falls back to the original when there is no segment', () => {
    expect(baseName('/')).toBe('/');
  });
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npx vitest run src/lib/group.test.ts`
Expected: FAIL — `baseName is not a function` / `not exported`.

- [ ] **Step 3: Implement `baseName`**

Append to `web/src/lib/group.ts`:

```ts
// baseName returns the last path segment of a grouping dir, for the pane title.
// A trailing slash is ignored. The '—' sentinel (unknown dir) and any input
// whose last segment is empty are returned unchanged.
export function baseName(dir: string): string {
  const trimmed = dir.replace(/\/+$/, '');
  const seg = trimmed.slice(trimmed.lastIndexOf('/') + 1);
  return seg || dir;
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npx vitest run src/lib/group.test.ts`
Expected: PASS (all `baseName` + existing cases green).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/group.ts web/src/lib/group.test.ts
git commit -m "feat(web): baseName helper for cockpit pane titles

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `quickAdd()` pure spawn helper

**Files:**
- Create: `web/src/lib/quickadd.ts`
- Test: `web/src/lib/quickadd.test.ts`

- [ ] **Step 1: Write the failing tests**

Create `web/src/lib/quickadd.test.ts`:

```ts
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { quickAdd } from './quickadd';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => { vi.restoreAllMocks(); });

describe('quickAdd', () => {
  it('spawns a no-prompt unsupervised agent in dir and returns the id', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-1' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    const out = await quickAdd('/work/project');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/spawn');
    expect(JSON.parse(opts.body)).toMatchObject({
      prompt: '', cwd: '/work/project', supervised: false, force: false,
    });
    expect(out).toEqual({ kind: 'created', id: 'agent-1' });
  });

  it('passes force through on retry', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ id: 'agent-2' }, 201));
    vi.stubGlobal('fetch', fetchMock);
    await quickAdd('/work/project', true);
    expect(JSON.parse(fetchMock.mock.calls[0][1].body).force).toBe(true);
  });

  it('maps a 428 memory-pressure verdict to a confirm result', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ verdict: { reason: 'memory at 92%' } }, 428),
    );
    vi.stubGlobal('fetch', fetchMock);
    const out = await quickAdd('/work/project');
    expect(out).toEqual({ kind: 'confirm', reason: 'memory at 92%' });
  });

  it('maps any other failure to an error result', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({ error: 'boom' }, 500),
    );
    vi.stubGlobal('fetch', fetchMock);
    const out = await quickAdd('/work/project');
    expect(out).toEqual({ kind: 'error', message: 'boom' });
  });
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npx vitest run src/lib/quickadd.test.ts`
Expected: FAIL — cannot find module `./quickadd`.

- [ ] **Step 3: Implement the helper**

Create `web/src/lib/quickadd.ts`:

```ts
import { spawn, ApiError, ConfirmationRequiredError } from './api';

// QuickAddResult is the discriminated outcome of a one-click pane spawn. The
// button maps each variant to UI state; quickAdd never throws.
export type QuickAddResult =
  | { kind: 'created'; id: string }
  | { kind: 'confirm'; reason: string } // 428 memory pressure — needs force
  | { kind: 'error'; message: string };

// quickAdd spawns a no-prompt, unsupervised agent in `dir`. Pass force=true to
// proceed past a memory-pressure 428 (a prior call returned { kind: 'confirm' }).
export async function quickAdd(dir: string, force = false): Promise<QuickAddResult> {
  try {
    const s = await spawn({ prompt: '', cwd: dir, supervised: false, force });
    return { kind: 'created', id: s.id };
  } catch (e) {
    if (e instanceof ConfirmationRequiredError) {
      return { kind: 'confirm', reason: e.verdict.reason };
    }
    const message = e instanceof ApiError ? e.message
      : e instanceof Error ? e.message : String(e);
    return { kind: 'error', message };
  }
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npx vitest run src/lib/quickadd.test.ts`
Expected: PASS (4 tests green).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/quickadd.ts web/src/lib/quickadd.test.ts
git commit -m "feat(web): quickAdd helper for no-prompt pane spawns

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: `QuickAddButton` component

**Files:**
- Create: `web/src/components/QuickAddButton.tsx`

No test file — this matches the codebase (components are thin shells; only
`lib/` logic is unit-tested). The spawn logic it calls is already covered by
Task 2.

- [ ] **Step 1: Create the component**

Create `web/src/components/QuickAddButton.tsx`:

```tsx
import { useState } from 'react';
import { quickAdd } from '../lib/quickadd';

// QuickAddButton is the per-pane '+' that spawns a no-prompt agent in `dir`.
// It owns its own busy / confirm / error state so AgentGrid stays presentational.
// A memory-pressure 428 flips it into a force state; the next click forces.
export default function QuickAddButton({ dir, onCreated }: {
  dir: string;
  onCreated: (id: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [confirmReason, setConfirmReason] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function click() {
    setBusy(true);
    setError(null);
    const res = await quickAdd(dir, confirmReason !== null);
    setBusy(false);
    if (res.kind === 'created') {
      setConfirmReason(null);
      onCreated(res.id);
    } else if (res.kind === 'confirm') {
      setConfirmReason(res.reason);
    } else {
      setError(res.message);
    }
  }

  const warn = confirmReason !== null || error !== null;
  const title = error
    ? `spawn failed: ${error}`
    : confirmReason
      ? `⚠ memory pressure: ${confirmReason} — click again to spawn anyway`
      : `Launch a new agent in ${dir}`;

  return (
    <button
      type="button"
      className={`grid-group-add${warn ? ' warn' : ''}`}
      disabled={busy}
      title={title}
      onClick={(e) => { e.stopPropagation(); click(); }}
    >
      {busy ? '…' : '+'}
    </button>
  );
}
```

- [ ] **Step 2: Verify it type-checks / builds**

Run: `cd web && npx astro check 2>/dev/null || npm run build`
Expected: build succeeds (the component is not yet imported anywhere, so this
only proves it compiles).
Note: `astro check` may not be configured; if it errors about an unknown command, the `npm run build` fallback is authoritative.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/QuickAddButton.tsx
git commit -m "feat(web): QuickAddButton pane control

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: AgentGrid titled panes + optional quick-add

**Files:**
- Modify: `web/src/components/AgentGrid.tsx`

- [ ] **Step 1: Rewrite AgentGrid**

Replace the entire contents of `web/src/components/AgentGrid.tsx` with:

```tsx
import type { Session } from '../lib/types';
import { groupSessions, baseName } from '../lib/group';
import MiniTerminal from './MiniTerminal';
import BusyIdleBadge from './BusyIdleBadge';
import QuickAddButton from './QuickAddButton';

// AgentGrid renders live thumbnail tiles for every agent, grouped by directory.
// Each directory group is a titled pane: a header bar (folder name + dim path +
// count [+ quick-add]) over the tile grid. Clicking a tile pins + focuses that
// agent. `lines` controls tile height (Cockpit passes a larger value than the
// Overview mini-grid). When `onCreated` is provided, each pane (except the
// unknown-dir '—' group) shows a '+' that spawns a no-prompt agent in its dir.
export default function AgentGrid({ sessions, onSelect, lines = 8, onCreated }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  lines?: number;
  onCreated?: (id: string) => void;
}) {
  if (sessions.length === 0) {
    return <p className="muted">No agents yet.</p>;
  }
  const groups = groupSessions(sessions);
  return (
    <div className="agent-grid-groups">
      {groups.map((g) => (
        <div key={g.dir} className="agent-grid-group">
          <div className="grid-group-bar">
            <span className="grid-group-name">{baseName(g.dir)}</span>
            <span className="grid-group-path">{g.dir}</span>
            <span className="grid-group-count">{g.sessions.length}</span>
            {onCreated && g.dir !== '—' && (
              <QuickAddButton dir={g.dir} onCreated={onCreated} />
            )}
          </div>
          <div className="agent-grid">
            {g.sessions.map((s) => (
              <button key={s.id} className="grid-tile" onClick={() => onSelect(s.id)}>
                <div className="tile-head">
                  <b>{s.id}</b> <BusyIdleBadge status={s.status} />
                </div>
                <MiniTerminal id={s.id} lines={lines} />
              </button>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
```

- [ ] **Step 2: Verify the build compiles**

Run: `cd web && npm run build`
Expected: build succeeds. (OverviewTab still calls AgentGrid without `onCreated`
— valid since the prop is optional.)

- [ ] **Step 3: Commit**

```bash
git add web/src/components/AgentGrid.tsx
git commit -m "feat(web): AgentGrid titled panes + optional per-pane quick-add

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Thread `onCreated` through CockpitTab and Dashboard

**Files:**
- Modify: `web/src/components/CockpitTab.tsx`
- Modify: `web/src/components/Dashboard.tsx`

- [ ] **Step 1: Add `onCreated` to CockpitTab**

Replace the entire contents of `web/src/components/CockpitTab.tsx` with:

```tsx
import type { Session } from '../lib/types';
import AgentGrid from './AgentGrid';

// CockpitTab is the full-size live grid (taller tiles than the Overview
// mini-grid). Clicking a pane pins + focuses that agent; the per-pane '+'
// (wired via onCreated) spawns a new agent in that pane's directory.
export default function CockpitTab({ sessions, onSelect, onCreated }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  onCreated: (id: string) => void;
}) {
  return (
    <div className="cockpit">
      <AgentGrid sessions={sessions} onSelect={onSelect} lines={14} onCreated={onCreated} />
    </div>
  );
}
```

- [ ] **Step 2: Pass `onCreated` from Dashboard**

In `web/src/components/Dashboard.tsx`, find the CockpitTab render (currently line 95):

```tsx
        {tabs.active === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} />}
```

Replace it with:

```tsx
        {tabs.active === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} onCreated={(id) => dispatch({ kind: 'open', id })} />}
```

(`dispatch({ kind: 'open', id })` is the same action the New-agent modal's
`onCreated` triggers — it pins + activates the new agent's tab.)

- [ ] **Step 3: Verify the build compiles**

Run: `cd web && npm run build`
Expected: build succeeds.

- [ ] **Step 4: Commit**

```bash
git add web/src/components/CockpitTab.tsx web/src/components/Dashboard.tsx
git commit -m "feat(web): wire cockpit quick-add to open the new agent's tab

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Pane + header bar styling

**Files:**
- Modify: `web/src/styles/app.css`

- [ ] **Step 1: Replace the agent-grid CSS block**

In `web/src/styles/app.css`, find the "Agent grid / cockpit" block (currently lines 85-92):

```css
/* ── Agent grid / cockpit ── */
.agent-grid-group { margin-bottom: 1rem; }
.grid-group-head { margin: .3rem 0; font-size: .8rem; }
.agent-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: .6rem; }
.grid-tile { text-align: left; padding: 0; border: 1px solid #8884; border-radius: .4rem; overflow: hidden; cursor: pointer; background: transparent; color: inherit; }
.grid-tile:hover { border-color: #2f81f7; }
.tile-head { display: flex; align-items: center; gap: .4rem; padding: .35rem .5rem; font-size: .85rem; background: #8881; }
.mini-term { margin: 0; background: #0b0b0b; color: #8fd98f; padding: .4rem; font-size: .72rem; line-height: 1.25; white-space: pre-wrap; overflow: hidden; }
```

Replace it with:

```css
/* ── Agent grid / cockpit ── */
.agent-grid-group { margin-bottom: 1rem; border: 1px solid #8884; border-radius: .5rem; overflow: hidden; }
.grid-group-bar { display: flex; align-items: center; gap: .5rem; padding: .4rem .6rem; background: #8881; border-bottom: 1px solid #8884; }
.grid-group-name { font-weight: 600; }
.grid-group-path { color: var(--idle); font-size: .8rem; flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.grid-group-count { color: var(--idle); font-size: .8rem; }
.grid-group-add { border: 1px solid #8884; border-radius: .3rem; background: transparent; color: inherit; cursor: pointer; line-height: 1; padding: .1rem .45rem; font-size: .95rem; }
.grid-group-add:hover:not(:disabled) { border-color: #2f81f7; }
.grid-group-add:disabled { opacity: .5; cursor: default; }
.grid-group-add.warn { border-color: var(--attention); color: var(--attention); }
.agent-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: .6rem; padding: .6rem; }
.grid-tile { text-align: left; padding: 0; border: 1px solid #8884; border-radius: .4rem; overflow: hidden; cursor: pointer; background: transparent; color: inherit; }
.grid-tile:hover { border-color: #2f81f7; }
.tile-head { display: flex; align-items: center; gap: .4rem; padding: .35rem .5rem; font-size: .85rem; background: #8881; }
.mini-term { margin: 0; background: #0b0b0b; color: #8fd98f; padding: .4rem; font-size: .72rem; line-height: 1.25; white-space: pre-wrap; overflow: hidden; }
```

(The old `.grid-group-head` rule is gone — the class is no longer rendered.)

- [ ] **Step 2: Verify no stray references to the removed class**

Run: `cd web && grep -rn "grid-group-head" src`
Expected: no matches.

- [ ] **Step 3: Verify the build compiles**

Run: `cd web && npm run build`
Expected: build succeeds.

- [ ] **Step 4: Commit**

```bash
git add web/src/styles/app.css
git commit -m "feat(web): titled-pane styling for cockpit agent groups

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the full web test suite**

Run: `cd web && npm test`
Expected: all suites PASS, including the new `group.test.ts` and `quickadd.test.ts` cases.

- [ ] **Step 2: Production build**

Run: `cd web && npm run build`
Expected: build succeeds and writes `web/dist`.

- [ ] **Step 3: Manual smoke (browser)**

The running daemon serves the embedded `web/dist`, so to see changes either run
the dev server or rebuild + reinstall the daemon. Quick path — dev server:

Run: `cd web && npm run dev` and open the printed local URL.
Verify:
- Cockpit tab: each directory group is a bordered pane with a header bar showing
  bold folder name, dim full path, count, and a `+`.
- Clicking `+` opens a new agent tab launched in that pane's directory.
- A group with dir `—` (if any) shows **no** `+`.
- Overview tab's "All agents" mini-grid shows the same panes but **no** `+`.

Note: to see it in the installed daemon (not the dev server) the user must
rebuild + reinstall (`make release && make install` or the repo's install
script) so the new `web/dist` is embedded — call this out at handoff; do not
restart the daemon without the user's go-ahead.

- [ ] **Step 4: Final commit (if any verification fixups were needed)**

Only if Step 1-3 surfaced fixes:

```bash
git add -A
git commit -m "fix(web): cockpit pane quick-add verification fixups

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Done criteria

- `baseName()` and `quickAdd()` unit-tested and green.
- Cockpit groups render as titled panes with clear headers and boundaries.
- Per-pane `+` spawns a no-prompt unsupervised agent in that pane's dir, opens
  its tab, handles a memory-pressure 428 with a force-retry, and is hidden on
  the `—` group and on Overview's mini-grid.
- `npm test` and `npm run build` both pass.
