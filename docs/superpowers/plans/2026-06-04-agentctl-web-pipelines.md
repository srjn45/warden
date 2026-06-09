# Web Pipelines Tab (Phase 6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A "Pipelines" tab in the web mission-control that lists pipelines, shows a selected pipeline's jobs as status-colored cards (with dependency chips), opens a per-job drawer (prompt / handoff / output + a link to the job's terminal tab), and offers cancel (pipeline) / retry (job) controls.

**Architecture:** A third fixed tab ('pipelines') in the existing tab reducer, rendered by a self-contained `PipelinesTab` React component that polls `GET /pipelines` (~2s) while mounted. Jobs are sessions, so "Open terminal" reuses the existing `onSelect(session_id)` → pins/activates the agent tab (the interactive `AttachTerminal`). Pure logic (types, api wrappers, status→class helpers, tab reducer) is unit-tested with vitest; the `.tsx` component is verified by the build (the repo unit-tests `lib/` only).

**Tech Stack:** Astro 5 + React 19 + TypeScript, vitest. No new dependencies. Module/build: `make web-test` (vitest), `make ui` (npm build → embedded via `go:embed`).

## Scope (mirrors the Phase 5 TUI decision)
**In this plan:** view (pipeline list + job cards + DAG-ish dependency chips + per-job drawer with read-only output) + **cancel** (pipeline) + **retry** (failed/needs_attention job) + a link to a running job's terminal.
**Deferred to a follow-up:** the in-browser **"New pipeline" builder** (add job cards + dependency multiselect → `pipeline create`). Authoring stays on `agentctl pipeline create -f spec.yaml`.
**Note on "live":** the existing SSE channel (`/events/stream`) carries sessions, not pipelines, so the tab polls `/pipelines` every ~2s while active (no background polling when another tab is shown). A true DAG graph with drawn edges is out of scope; jobs render as cards with dependency chips (the same at-a-glance the TUI uses).

---

## File Structure

- **Modify** `web/src/lib/types.ts` — `Pipeline`, `PipelineJob`, `PipelineStatus`, `PipelineJobStatus` types.
- **Modify** `web/src/lib/api.ts` — `listPipelines`, `cancelPipeline`, `retryJob`.
- **Modify** `web/src/lib/api.test.ts` — tests for the three calls.
- **Create** `web/src/lib/pipelines.ts` — pure `jobStatusClass`, `isJobRetryable`.
- **Create** `web/src/lib/pipelines.test.ts` — tests.
- **Modify** `web/src/lib/tabs.ts` — add `'pipelines'` as a fixed tab in prune validity.
- **Modify** `web/src/lib/tabs.test.ts` — test prune keeps `'pipelines'`.
- **Create** `web/src/components/PipelinesTab.tsx` — the tab UI.
- **Modify** `web/src/components/TabBar.tsx` — the Pipelines tab button.
- **Modify** `web/src/components/Dashboard.tsx` — render `PipelinesTab` when active; fix the empty-fallback guard.
- **Modify** `web/src/styles/app.css` — pipeline list / job cards / drawer styles.

---

## Task 1: Pipeline types + API client

**Files:**
- Modify: `web/src/lib/types.ts`
- Modify: `web/src/lib/api.ts`
- Test: `web/src/lib/api.test.ts`

- [ ] **Step 1: Write the failing test**

Append to `web/src/lib/api.test.ts` (and add `listPipelines, cancelPipeline, retryJob` to the import from `'./api'` at the top):

```ts
describe('pipelines api', () => {
  it('listPipelines GETs /pipelines and unwraps the array', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ pipelines: [{ id: 'demo', jobs: [] }] }));
    vi.stubGlobal('fetch', fetchMock);
    const out = await listPipelines();
    expect(fetchMock).toHaveBeenCalledWith('/pipelines');
    expect(out).toHaveLength(1);
    expect(out[0].id).toBe('demo');
  });

  it('listPipelines returns [] when the body is null', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ pipelines: null })));
    expect(await listPipelines()).toEqual([]);
  });

  it('cancelPipeline POSTs to the cancel endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'canceled' }));
    vi.stubGlobal('fetch', fetchMock);
    await cancelPipeline('demo');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/pipelines/demo/cancel');
    expect(opts.method).toBe('POST');
  });

  it('retryJob POSTs to the job retry endpoint', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'retrying' }));
    vi.stubGlobal('fetch', fetchMock);
    await retryJob('demo', 'a');
    expect(fetchMock.mock.calls[0][0]).toBe('/pipelines/demo/jobs/a/retry');
    expect(fetchMock.mock.calls[0][1].method).toBe('POST');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run src/lib/api.test.ts`
Expected: FAIL — `listPipelines`/`cancelPipeline`/`retryJob` are not exported.

- [ ] **Step 3: Add the types + api functions**

In `web/src/lib/types.ts`, append:

```ts
export type PipelineStatus = 'pending' | 'running' | 'done' | 'stalled' | 'canceled';
export type PipelineJobStatus =
  | 'pending' | 'running' | 'done' | 'failed' | 'skipped' | 'needs_attention';

export interface PipelineJob {
  id: string;
  prompt: string;
  depends_on: string[] | null;
  handoff: string;
  worktree: string;
  supervised: boolean;
  type: string;
  session_id: string;
  status: PipelineJobStatus;
  output: string;
  branch: string;
}

export interface Pipeline {
  id: string;
  name: string;
  repo: string;
  status: PipelineStatus;
  jobs: PipelineJob[];
}
```

In `web/src/lib/api.ts`, add `Pipeline` to the type import and append the functions:

```ts
export async function listPipelines(): Promise<Pipeline[]> {
  const data = await parse<{ pipelines: Pipeline[] | null }>(await fetch('/pipelines'));
  return data.pipelines ?? [];
}

export async function cancelPipeline(id: string): Promise<void> {
  await parse<unknown>(await fetch(`/pipelines/${encodeURIComponent(id)}/cancel`, { method: 'POST' }));
}

export async function retryJob(pid: string, job: string): Promise<void> {
  await parse<unknown>(await fetch(
    `/pipelines/${encodeURIComponent(pid)}/jobs/${encodeURIComponent(job)}/retry`,
    { method: 'POST' },
  ));
}
```

(Update the first line of `api.ts` to `import type { Session, ApprovalView, Pipeline } from './types';`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test -- --run src/lib/api.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/api.ts web/src/lib/api.test.ts
git commit -m "feat(web): pipeline types + listPipelines/cancelPipeline/retryJob"
```

---

## Task 2: Pure pipeline UI helpers

**Files:**
- Create: `web/src/lib/pipelines.ts`
- Test: `web/src/lib/pipelines.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/pipelines.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { jobStatusClass, isJobRetryable } from './pipelines';

describe('pipelines helpers', () => {
  it('jobStatusClass maps each status to a distinct class', () => {
    const statuses = ['pending', 'running', 'done', 'failed', 'skipped', 'needs_attention'] as const;
    const classes = statuses.map(jobStatusClass);
    expect(new Set(classes).size).toBe(statuses.length); // all distinct
    expect(classes.every((c) => c.startsWith('job-'))).toBe(true);
  });

  it('isJobRetryable is true only for failed/needs_attention', () => {
    expect(isJobRetryable('failed')).toBe(true);
    expect(isJobRetryable('needs_attention')).toBe(true);
    expect(isJobRetryable('running')).toBe(false);
    expect(isJobRetryable('pending')).toBe(false);
    expect(isJobRetryable('done')).toBe(false);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run src/lib/pipelines.test.ts`
Expected: FAIL — module `./pipelines` not found.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/pipelines.ts`:

```ts
import type { PipelineJobStatus } from './types';

// jobStatusClass returns the CSS class for a job card (styled in app.css).
export function jobStatusClass(s: PipelineJobStatus): string {
  return `job-${s}`;
}

// isJobRetryable reports whether `pipeline retry` applies to this job.
export function isJobRetryable(s: PipelineJobStatus): boolean {
  return s === 'failed' || s === 'needs_attention';
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test -- --run src/lib/pipelines.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/pipelines.ts web/src/lib/pipelines.test.ts
git commit -m "feat(web): pure pipeline job-status helpers"
```

---

## Task 3: Add the 'pipelines' fixed tab to the reducer

**Files:**
- Modify: `web/src/lib/tabs.ts`
- Test: `web/src/lib/tabs.test.ts`

- [ ] **Step 1: Write the failing test**

Append to `web/src/lib/tabs.test.ts`:

```ts
it('prune keeps the pipelines fixed tab active', () => {
  const s = { pinned: [], active: 'pipelines' };
  const out = tabsReducer(s, { kind: 'prune', alive: [] });
  expect(out.active).toBe('pipelines');
});
```

(If the test file doesn't already import `tabsReducer`, add it to the existing import from `'./tabs'`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npm test -- --run src/lib/tabs.test.ts`
Expected: FAIL — prune resets `'pipelines'` to `'overview'` (it's not yet a recognized fixed tab).

- [ ] **Step 3: Update the reducer**

In `web/src/lib/tabs.ts`, in the `'prune'` case, add `'pipelines'` to the fixed-tab validity check:

```ts
    case 'prune': {
      const alive = new Set(a.alive);
      const pinned = s.pinned.filter((id) => alive.has(id));
      const active = pinned.includes(s.active) || s.active === 'overview' || s.active === 'cockpit' || s.active === 'pipelines'
        ? s.active
        : 'overview';
      return { pinned, active };
    }
```

Also update the leading comment to mention three fixed tabs:

```ts
// The shell has three fixed tabs ('overview', 'cockpit', 'pipelines') that always
// exist, plus zero or more pinned agent tabs (keyed by agent id). `active` is
// whichever tab is showing — a fixed id or a pinned agent id.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npm test -- --run src/lib/tabs.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/tabs.ts web/src/lib/tabs.test.ts
git commit -m "feat(web): pipelines as a third fixed tab"
```

---

## Task 4: PipelinesTab component + tab wiring + styles

**Files:**
- Create: `web/src/components/PipelinesTab.tsx`
- Modify: `web/src/components/TabBar.tsx`
- Modify: `web/src/components/Dashboard.tsx`
- Modify: `web/src/styles/app.css`

(No unit test — the repo unit-tests `lib/` only; components are verified by the build in Task 5. This task's "test" is a clean `npm run build`.)

- [ ] **Step 1: Create the component**

Create `web/src/components/PipelinesTab.tsx`:

```tsx
import { useEffect, useState } from 'react';
import type { Pipeline, PipelineJob } from '../lib/types';
import { listPipelines, cancelPipeline, retryJob } from '../lib/api';
import { jobStatusClass, isJobRetryable } from '../lib/pipelines';

// PipelinesTab polls /pipelines while mounted (the SSE channel carries sessions,
// not pipelines). Jobs are sessions, so "Open terminal" reuses onSelect to pin
// the agent tab. Read-only view + cancel/retry; authoring is via the CLI.
export default function PipelinesTab({ onSelect }: { onSelect: (id: string) => void }) {
  const [pipelines, setPipelines] = useState<Pipeline[]>([]);
  const [selId, setSelId] = useState<string | null>(null);
  const [jobId, setJobId] = useState<string | null>(null);

  useEffect(() => {
    let on = true;
    const load = () => listPipelines().then((ps) => { if (on) setPipelines(ps); }).catch(() => { /* keep last */ });
    load();
    const t = setInterval(load, 2000);
    return () => { on = false; clearInterval(t); };
  }, []);

  const selected = pipelines.find((p) => p.id === selId) ?? pipelines[0] ?? null;
  const drawerJob: PipelineJob | null = selected && jobId
    ? selected.jobs.find((j) => j.id === jobId) ?? null
    : null;

  return (
    <div className="pipelines">
      <aside className="pipe-list">
        {pipelines.length === 0 && (
          <div className="empty">No pipelines yet. Create one with <code>agentctl pipeline create -f spec.yaml</code>.</div>
        )}
        {pipelines.map((p) => (
          <button
            key={p.id}
            className={`pipe-item${p.id === selected?.id ? ' on' : ''}`}
            onClick={() => { setSelId(p.id); setJobId(null); }}
          >
            <span className="pipe-name">{p.id}</span>
            <span className={`pipe-status st-${p.status}`}>{p.status}</span>
          </button>
        ))}
      </aside>

      {selected && (
        <section className="pipe-detail">
          <header className="pipe-head">
            <h2>{selected.id} <span className={`pipe-status st-${selected.status}`}>{selected.status}</span></h2>
            <button className="btn" onClick={() => cancelPipeline(selected.id).catch(() => { /* ignore */ })}>Cancel</button>
          </header>
          <div className="job-grid">
            {selected.jobs.map((j) => (
              <button
                key={j.id}
                className={`job-card ${jobStatusClass(j.status)}${j.id === jobId ? ' on' : ''}`}
                onClick={() => setJobId(j.id)}
              >
                <div className="job-id">{j.id}</div>
                <div className="job-st">{j.status}</div>
                {j.depends_on && j.depends_on.length > 0 && (
                  <div className="job-deps">← {j.depends_on.join(', ')}</div>
                )}
              </button>
            ))}
          </div>
        </section>
      )}

      {drawerJob && selected && (
        <aside className="job-drawer">
          <header className="drawer-head">
            <h3>{drawerJob.id} <span className={jobStatusClass(drawerJob.status)}>{drawerJob.status}</span></h3>
            <button className="tab-close" title="Close" onClick={() => setJobId(null)}>✕</button>
          </header>
          <label>Prompt</label>
          <pre className="job-text">{drawerJob.prompt}</pre>
          {drawerJob.handoff && (<><label>Handoff hint</label><pre className="job-text">{drawerJob.handoff}</pre></>)}
          {drawerJob.output && (<><label>Output</label><pre className="job-text">{drawerJob.output}</pre></>)}
          <div className="drawer-actions">
            {drawerJob.session_id && drawerJob.status === 'running' && (
              <button className="btn" onClick={() => onSelect(drawerJob.session_id)}>Open terminal</button>
            )}
            {isJobRetryable(drawerJob.status) && (
              <button className="btn" onClick={() => retryJob(selected.id, drawerJob.id).catch(() => { /* ignore */ })}>Retry</button>
            )}
          </div>
        </aside>
      )}
    </div>
  );
}
```

- [ ] **Step 2: Add the tab button**

In `web/src/components/TabBar.tsx`, add a Pipelines button after the Cockpit button:

```tsx
      <button className={cls('cockpit')} onClick={() => onActivate('cockpit')}>⊞ Cockpit</button>
      <button className={cls('pipelines')} onClick={() => onActivate('pipelines')}>⛓ Pipelines</button>
```

- [ ] **Step 3: Wire it into the Dashboard**

In `web/src/components/Dashboard.tsx`, import the component:

```tsx
import PipelinesTab from './PipelinesTab';
```

Add a render line in `<main className="tab-content">` after the cockpit line:

```tsx
        {tabs.active === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} />}
        {tabs.active === 'pipelines' && <PipelinesTab onSelect={select} />}
```

And update the empty-fallback guard so `'pipelines'` isn't treated as an ended agent:

```tsx
        {tabs.active !== 'overview' && tabs.active !== 'cockpit' && tabs.active !== 'pipelines' && !activeSession && (
          <div className="detail empty">That agent has ended.</div>
        )}
```

- [ ] **Step 4: Add styles**

Append to `web/src/styles/app.css`:

```css
/* Pipelines tab */
.pipelines { display: flex; gap: 12px; height: 100%; min-height: 0; }
.pipe-list { width: 220px; flex: none; overflow-y: auto; display: flex; flex-direction: column; gap: 4px; }
.pipe-item { display: flex; justify-content: space-between; gap: 8px; padding: 6px 8px; background: #1b1b1f; border: 1px solid #2a2a30; border-radius: 6px; color: inherit; cursor: pointer; text-align: left; }
.pipe-item.on { border-color: #4a7; }
.pipe-status { font-size: 11px; opacity: 0.8; }
.pipe-status.st-running { color: #6cf; }
.pipe-status.st-done { color: #4a7; }
.pipe-status.st-stalled { color: #e84; }
.pipe-status.st-canceled, .pipe-status.st-pending { color: #999; }
.pipe-detail { flex: 1; min-width: 0; overflow-y: auto; }
.pipe-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.job-grid { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 10px; }
.job-card { width: 150px; text-align: left; padding: 8px; border-radius: 6px; border: 1px solid #2a2a30; background: #1b1b1f; color: inherit; cursor: pointer; }
.job-card.on { outline: 2px solid #6cf; }
.job-id { font-weight: 600; }
.job-st { font-size: 11px; opacity: 0.85; }
.job-deps { font-size: 11px; opacity: 0.6; margin-top: 4px; }
.job-running { border-left: 3px solid #6cf; }
.job-done { border-left: 3px solid #4a7; }
.job-failed { border-left: 3px solid #e55; }
.job-needs_attention { border-left: 3px solid #e84; }
.job-skipped { border-left: 3px solid #555; opacity: 0.6; }
.job-pending { border-left: 3px solid #888; }
.job-drawer { width: 320px; flex: none; overflow-y: auto; border-left: 1px solid #2a2a30; padding-left: 12px; display: flex; flex-direction: column; gap: 6px; }
.drawer-head { display: flex; align-items: center; justify-content: space-between; }
.job-text { white-space: pre-wrap; word-break: break-word; background: #141417; padding: 8px; border-radius: 6px; max-height: 240px; overflow-y: auto; }
.drawer-actions { display: flex; gap: 8px; margin-top: 8px; }
```

(If a `.btn` style does not already exist in `app.css`, the buttons fall back to the browser default — that's acceptable; do NOT invent unrelated global button styling.)

- [ ] **Step 5: Build to verify it compiles + commit**

Run: `cd web && npm run build`
Expected: build completes (no TypeScript/Vite errors).

```bash
git add web/src/components/PipelinesTab.tsx web/src/components/TabBar.tsx web/src/components/Dashboard.tsx web/src/styles/app.css
git commit -m "feat(web): Pipelines tab (job cards, drawer, cancel/retry, terminal link)"
```

---

## Task 5: Docs + full verification

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Add a note**

In `docs/USAGE.md`, near the web/GUI section (or the Pipelines section), add:

```markdown
The web dashboard has a **Pipelines** tab: it lists pipelines, shows a selected
pipeline's jobs as status-colored cards with dependency chips, and a per-job
drawer with the prompt/handoff/output, a **Cancel** (pipeline) / **Retry** (job)
control, and an **Open terminal** link to a running job's session. (Creating /
editing pipelines in the browser is not yet available — use `agentctl pipeline
create -f`.)
```

- [ ] **Step 2: Full verification**

Run: `cd web && npm test -- --run` (all vitest green)
Run: `cd web && npm run build` (clean build)
Run (from repo root): `go build ./... && go test ./... && make lint`
Expected: all pass; lint clean. If anything fails, do NOT commit — report it.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document the web Pipelines tab"
```

---

## Verification checklist (after all tasks)

- [ ] `cd web && npm test -- --run` green; `cd web && npm run build` clean.
- [ ] `go build ./... && go test ./...` green; `make lint` clean.
- [ ] Manual smoke (rebuild + restart daemon: `./scripts/reinstall.sh`, which runs `make release` → embeds the new UI), with a created pipeline:
  - Open the dashboard (`http://127.0.0.1:8765`), click **⛓ Pipelines**.
  - The pipeline appears in the left list with its status; selecting it shows the job cards (colored by status, with dependency chips).
  - Click a job card → drawer shows prompt/output; for a running job, **Open terminal** pins/activates its agent tab (live xterm).
  - **Cancel** on the pipeline header and **Retry** on a failed/needs-attention job's drawer work (state updates within ~2s via polling).

## Deferred to a follow-up (not in this plan)
- The in-browser **"New pipeline" builder** (add job cards + dependency multiselect → `pipeline create`). Authoring stays on `agentctl pipeline create -f spec.yaml`.
- Drawn DAG edges / auto-layout graph (jobs render as cards with dependency chips instead).
- Editing a pending job's prompt in the browser.
```
