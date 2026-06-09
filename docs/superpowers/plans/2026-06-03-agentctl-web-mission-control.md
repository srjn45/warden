# agentctl Web Mission-Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the agentctl web UI as a tabbed "mission control" dashboard — Overview / Cockpit / pinned agent tabs — with a multi-pane cockpit, live colored terminal streaming (xterm.js + a new output SSE endpoint), an attention queue with hidden-tab-gated notifications, and a fixed (currently broken) Terminate flow.

**Architecture:** One new Go SSE endpoint `GET /sessions/{id}/output/stream` pushes `tmux capture-pane -e` (ANSI-colored) snapshots ~1s, JSON-framed. The React frontend becomes a thin shell (`Dashboard`) hosting an always-visible `AttentionBar`, a `TabBar`, and the active tab. Pure, testable logic lives in `web/src/lib/*` (tabs reducer, attention/stats/activity derivations, notify trigger, api calls); components stay thin and consume it.

**Tech Stack:** Go (chi, SSE), Astro + React 19, xterm.js (`@xterm/xterm` + `@xterm/addon-fit`), Vitest (jsdom).

**Testing approach note:** This repo's web tests cover **pure lib functions only** — there are no component-render tests and no `@testing-library` dependency. This plan keeps that convention: all new logic is unit-tested in `web/src/lib/*.test.ts`, the new Go handler gets a `daemon` test, and React components are verified via `npm run build` + a final manual smoke. We deliberately do **not** add component-render tests (a justified deviation from the spec's "jsdom component tests", to avoid a new dependency and test style).

---

## File Structure

**Backend (Go):**
- Modify `internal/lifecycle/lifecycle.go` — add `OutputANSI` (capture-pane with `-e`).
- Modify `internal/daemon/api.go` — add `OutputANSI` to the `Lifecycle` interface.
- Modify `internal/daemon/lifecycle_adapter.go` — forward `OutputANSI`.
- Modify `internal/daemon/lifecycle_routes_test.go` — add `OutputANSI` to `fakeLife`.
- Create `internal/daemon/output_stream.go` — the SSE output-stream handler.
- Modify `internal/daemon/lifecycle_routes.go` — register the route.
- Create `internal/daemon/output_stream_test.go` — handler test.

**Frontend lib (pure, unit-tested):**
- Modify `web/src/lib/api.ts` + `web/src/lib/api.test.ts` — terminate/removeWorktree/deleteSession + `subscribeOutput`.
- Create `web/src/lib/tabs.ts` + `web/src/lib/tabs.test.ts`.
- Create `web/src/lib/attention.ts` + `web/src/lib/attention.test.ts`.
- Create `web/src/lib/stats.ts` + `web/src/lib/stats.test.ts`.
- Create `web/src/lib/activity.ts` + `web/src/lib/activity.test.ts`.
- Create `web/src/lib/notify.ts` + `web/src/lib/notify.test.ts`.

**Frontend components (thin, build-verified):**
- Create `Terminal.tsx`, `MiniTerminal.tsx`, `AttentionBar.tsx`, `TabBar.tsx`, `AttentionQueue.tsx`, `FleetStats.tsx`, `AgentGrid.tsx`, `ActivityFeed.tsx`, `QuickSpawn.tsx`, `OverviewTab.tsx`, `CockpitTab.tsx`, `AgentTab.tsx`.
- Rework `TerminateControls.tsx`, `Dashboard.tsx`.
- Modify `web/package.json` (xterm deps), `web/src/styles/app.css` (new styles).

---

## Task 1: Backend — `OutputANSI` (colored pane capture)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (after `Output`, ~line 627)
- Modify: `internal/daemon/api.go:99`
- Modify: `internal/daemon/lifecycle_adapter.go:77`
- Modify: `internal/daemon/lifecycle_routes_test.go` (`fakeLife`, ~line 88)

- [ ] **Step 1: Add `OutputANSI` to the lifecycle implementation**

In `internal/lifecycle/lifecycle.go`, immediately after the existing `Output` method, add:

```go
// OutputANSI is like Output but preserves ANSI escape sequences (tmux -e), so
// the web terminal can render the pane in color.
func (l *Lifecycle) OutputANSI(ctx context.Context, tmuxSession string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	out, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-e", "-t", tmuxSession, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w: %s", err, out)
	}
	return out, nil
}
```

- [ ] **Step 2: Add `OutputANSI` to the `Lifecycle` interface**

In `internal/daemon/api.go`, in the `Lifecycle interface` block, directly below the existing `Output(...)` line (line 99), add:

```go
	OutputANSI(ctx context.Context, tmuxSession string, lines int) (string, error)
```

- [ ] **Step 3: Forward `OutputANSI` in the adapter**

In `internal/daemon/lifecycle_adapter.go`, after the existing `Output` method (~line 79), add:

```go
func (a *lifecycleAdapter) OutputANSI(ctx context.Context, tmuxSession string, lines int) (string, error) {
	return a.lc.OutputANSI(ctx, tmuxSession, lines)
}
```

- [ ] **Step 4: Add `OutputANSI` to `fakeLife` in tests**

In `internal/daemon/lifecycle_routes_test.go`, directly after the existing `fakeLife.Output` method (~line 90), add:

```go
func (f *fakeLife) OutputANSI(_ context.Context, s string, n int) (string, error) {
	return f.output, nil
}
```

- [ ] **Step 5: Verify everything still compiles and tests pass**

Run: `go build ./... && go test ./internal/daemon/ ./internal/lifecycle/`
Expected: build succeeds; existing tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): add OutputANSI (colored tmux capture) to lifecycle"
```

---

## Task 2: Backend — SSE output stream endpoint

**Files:**
- Create: `internal/daemon/output_stream.go`
- Modify: `internal/daemon/lifecycle_routes.go:24` (register route)
- Test: `internal/daemon/output_stream_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/output_stream_test.go`:

```go
package daemon

import (
	"bufio"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestOutputStreamSendsFrame(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking})
	fl := &fakeLife{output: "\x1b[32mok\x1b[0m internal/tui"}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/A-1/output/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// First frame is sent immediately; read until a blank-line-terminated event.
	r := bufio.NewReader(resp.Body)
	frame := readEvent(t, r) // reuse helper from sse_test.go
	require.Contains(t, frame, `internal/tui`)
	require.Contains(t, frame, `[32mok`, "ANSI escapes survive JSON framing")
	_ = time.Second
}

func TestOutputStreamNotFound(t *testing.T) {
	fs := newFakeStore()
	fl := &fakeLife{}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/nope/output/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestOutputStream -v`
Expected: FAIL — `s.router()` has no `/sessions/{id}/output/stream` route (404 for the first test; compile error if `handleOutputStream` referenced before it exists — it isn't yet, so the test gets 404).

- [ ] **Step 3: Write the SSE handler**

Create `internal/daemon/output_stream.go`:

```go
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/store"
)

// outputStreamInterval is how often the handler re-captures the agent's pane.
const outputStreamInterval = time.Second

// handleOutputStream streams a single agent's tmux pane as SSE. Each frame is a
// JSON-encoded OutputResponse (so embedded newlines / ANSI escapes survive SSE
// line framing). It sends an immediate first frame, then a new one whenever the
// pane changes, on a ~1s tick.
func (s *Server) handleOutputStream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	var last string
	send := func() bool {
		out, err := s.life.OutputANSI(r.Context(), sess.TmuxSession, 200)
		if err != nil {
			return true // transient (e.g. pane gone mid-poll); try again next tick
		}
		if out == last {
			return true
		}
		last = out
		payload, err := json.Marshal(OutputResponse{Output: out})
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send() {
		return
	}
	ticker := time.NewTicker(outputStreamInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}
```

- [ ] **Step 4: Register the route**

In `internal/daemon/lifecycle_routes.go`, in the function that registers lifecycle routes, directly after the existing `r.Get("/sessions/{id}/output", s.handleOutput)` line (line 24), add:

```go
	r.Get("/sessions/{id}/output/stream", s.handleOutputStream)
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestOutputStream -v`
Expected: PASS (both `TestOutputStreamSendsFrame` and `TestOutputStreamNotFound`).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/output_stream.go internal/daemon/output_stream_test.go internal/daemon/lifecycle_routes.go
git commit -m "feat(daemon): SSE output stream endpoint for live colored terminal"
```

---

## Task 3: Frontend — rework `api.ts` (fix Terminate + add output stream)

**Files:**
- Modify: `web/src/lib/api.ts` (replace `cleanup`, add helpers)
- Test: `web/src/lib/api.test.ts`

- [ ] **Step 1: Update the failing tests**

In `web/src/lib/api.test.ts`, change the import line to drop `cleanup` and add the new functions:

```ts
import { listSessions, spawn, listDirs, terminate, removeWorktree, deleteSession, ApiError } from './api';
```

Replace the existing `it('cleanup POSTs id/force/hard to /cleanup', ...)` test with:

```ts
  it('terminate POSTs to /sessions/{id}/terminate', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'terminated' }));
    vi.stubGlobal('fetch', fetchMock);
    await terminate('A-1');
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/sessions/A-1/terminate');
    expect(opts.method).toBe('POST');
  });

  it('removeWorktree POSTs force to /sessions/{id}/remove-worktree', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'worktree removed' }));
    vi.stubGlobal('fetch', fetchMock);
    await removeWorktree('A-1', true);
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/sessions/A-1/remove-worktree');
    expect(JSON.parse(opts.body)).toEqual({ force: true });
  });

  it('deleteSession POSTs hard to /sessions/{id}/delete', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ status: 'deleted' }));
    vi.stubGlobal('fetch', fetchMock);
    await deleteSession('A-1', true);
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/sessions/A-1/delete');
    expect(JSON.parse(opts.body)).toEqual({ hard: true });
  });
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd web && npx vitest run src/lib/api.test.ts`
Expected: FAIL — `terminate`, `removeWorktree`, `deleteSession` are not exported.

- [ ] **Step 3: Replace `cleanup` with the real endpoint helpers and add the output stream subscriber**

In `web/src/lib/api.ts`, delete the entire `cleanup` function and replace it with:

```ts
export async function terminate(id: string): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/terminate`, {
    method: 'POST',
  }));
}

export async function removeWorktree(id: string, force: boolean): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/remove-worktree`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ force }),
  }));
}

export async function deleteSession(id: string, hard: boolean): Promise<void> {
  await parse<unknown>(await fetch(`/sessions/${encodeURIComponent(id)}/delete`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ hard }),
  }));
}
```

Then, at the end of the file (after `subscribeSessions`), add the per-agent output stream subscriber:

```ts
// subscribeOutput opens an SSE connection to an agent's live pane. Each frame is
// a JSON OutputResponse; onFrame receives the decoded pane text. Returns an
// unsubscribe function.
export function subscribeOutput(
  id: string,
  onFrame: (output: string) => void,
  onError?: () => void,
): () => void {
  const es = new EventSource(`/sessions/${encodeURIComponent(id)}/output/stream`);
  es.onmessage = (e) => {
    try {
      const d = JSON.parse(e.data) as { output: string };
      onFrame(d.output ?? '');
    } catch { /* ignore malformed frame */ }
  };
  es.onerror = () => onError?.();
  return () => es.close();
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd web && npx vitest run src/lib/api.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/api.ts web/src/lib/api.test.ts
git commit -m "fix(web): wire terminate to real endpoints; add output stream subscriber"
```

---

## Task 4: Frontend — `tabs.ts` reducer

**Files:**
- Create: `web/src/lib/tabs.ts`
- Test: `web/src/lib/tabs.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/tabs.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { tabsReducer, initialTabs, type TabsState } from './tabs';

describe('tabsReducer', () => {
  it('open pins an agent and activates it', () => {
    const s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    expect(s.pinned).toEqual(['A-1']);
    expect(s.active).toBe('A-1');
  });

  it('open is idempotent on the pinned list but re-activates', () => {
    let s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    s = tabsReducer(s, { kind: 'activate', id: 'overview' });
    s = tabsReducer(s, { kind: 'open', id: 'A-1' });
    expect(s.pinned).toEqual(['A-1']);
    expect(s.active).toBe('A-1');
  });

  it('activate switches the active tab without changing pins', () => {
    let s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    s = tabsReducer(s, { kind: 'activate', id: 'cockpit' });
    expect(s.active).toBe('cockpit');
    expect(s.pinned).toEqual(['A-1']);
  });

  it('close removes a pin and falls back to overview when it was active', () => {
    let s = tabsReducer(initialTabs, { kind: 'open', id: 'A-1' });
    s = tabsReducer(s, { kind: 'close', id: 'A-1' });
    expect(s.pinned).toEqual([]);
    expect(s.active).toBe('overview');
  });

  it('close keeps the active tab when a different pin is closed', () => {
    let s: TabsState = { pinned: ['A-1', 'B-2'], active: 'B-2' };
    s = tabsReducer(s, { kind: 'close', id: 'A-1' });
    expect(s.pinned).toEqual(['B-2']);
    expect(s.active).toBe('B-2');
  });

  it('prune drops pins for agents that no longer exist', () => {
    let s: TabsState = { pinned: ['A-1', 'B-2'], active: 'A-1' };
    s = tabsReducer(s, { kind: 'prune', alive: ['B-2'] });
    expect(s.pinned).toEqual(['B-2']);
    expect(s.active).toBe('overview'); // active pin vanished → fall back
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/tabs.test.ts`
Expected: FAIL — cannot find module `./tabs`.

- [ ] **Step 3: Write the reducer**

Create `web/src/lib/tabs.ts`:

```ts
// The shell has two fixed tabs ('overview', 'cockpit') that always exist, plus
// zero or more pinned agent tabs (keyed by agent id). `active` is whichever tab
// is showing — a fixed id or a pinned agent id.

export interface TabsState {
  pinned: string[]; // agent ids, in open order
  active: string;   // 'overview' | 'cockpit' | <agent id>
}

export type TabsAction =
  | { kind: 'open'; id: string }      // pin (if new) + activate an agent
  | { kind: 'close'; id: string }     // unpin an agent
  | { kind: 'activate'; id: string }  // switch active tab
  | { kind: 'prune'; alive: string[] }; // drop pins not in `alive`

export const initialTabs: TabsState = { pinned: [], active: 'overview' };

export function tabsReducer(s: TabsState, a: TabsAction): TabsState {
  switch (a.kind) {
    case 'open': {
      const pinned = s.pinned.includes(a.id) ? s.pinned : [...s.pinned, a.id];
      return { pinned, active: a.id };
    }
    case 'activate':
      return { ...s, active: a.id };
    case 'close': {
      const pinned = s.pinned.filter((id) => id !== a.id);
      const active = s.active === a.id ? 'overview' : s.active;
      return { pinned, active };
    }
    case 'prune': {
      const alive = new Set(a.alive);
      const pinned = s.pinned.filter((id) => alive.has(id));
      const active = pinned.includes(s.active) || s.active === 'overview' || s.active === 'cockpit'
        ? s.active
        : 'overview';
      return { pinned, active };
    }
    default:
      return s;
  }
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/tabs.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/tabs.ts web/src/lib/tabs.test.ts
git commit -m "feat(web): tabs reducer for the mission-control shell"
```

---

## Task 5: Frontend — `attention.ts`

**Files:**
- Create: `web/src/lib/attention.ts`
- Test: `web/src/lib/attention.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/attention.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { needsAttention } from './attention';
import type { Session } from './types';

const sess = (id: string, status: Session['status']): Session =>
  ({ id, status } as Session);

describe('needsAttention', () => {
  it('selects waiting_for_input, errored, and orphaned agents', () => {
    const sessions = [
      sess('A-1', 'working'),
      sess('B-2', 'waiting_for_input'),
      sess('C-3', 'errored'),
      sess('D-4', 'idle'),
      sess('E-5', 'orphaned'),
    ];
    expect(needsAttention(sessions).map((s) => s.id)).toEqual(['B-2', 'C-3', 'E-5']);
  });

  it('returns an empty array when nothing needs attention', () => {
    expect(needsAttention([sess('A-1', 'working')])).toEqual([]);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/attention.test.ts`
Expected: FAIL — cannot find module `./attention`.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/attention.ts`:

```ts
import type { Session } from './types';

// needsAttention selects agents that are blocked on the user or have failed:
// waiting_for_input (blocked), errored / orphaned (failed). Input order is kept.
export function needsAttention(sessions: Session[]): Session[] {
  return sessions.filter(
    (s) => s.status === 'waiting_for_input' || s.status === 'errored' || s.status === 'orphaned',
  );
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/attention.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/attention.ts web/src/lib/attention.test.ts
git commit -m "feat(web): needsAttention selector"
```

---

## Task 6: Frontend — `stats.ts`

**Files:**
- Create: `web/src/lib/stats.ts`
- Test: `web/src/lib/stats.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/stats.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { deriveFleetStats } from './stats';
import type { Session } from './types';

const sess = (id: string, status: Session['status']): Session =>
  ({ id, status } as Session);

describe('deriveFleetStats', () => {
  it('counts total/busy/waiting/errored buckets', () => {
    const sessions = [
      sess('A-1', 'working'),
      sess('A-2', 'spawning'),
      sess('B-2', 'waiting_for_input'),
      sess('C-3', 'errored'),
      sess('D-4', 'orphaned'),
      sess('E-5', 'idle'),
      sess('F-6', 'done'),
    ];
    expect(deriveFleetStats(sessions)).toEqual({
      total: 7, busy: 2, waiting: 1, errored: 2,
    });
  });

  it('is all-zero for an empty fleet', () => {
    expect(deriveFleetStats([])).toEqual({ total: 0, busy: 0, waiting: 0, errored: 0 });
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/stats.test.ts`
Expected: FAIL — cannot find module `./stats`.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/stats.ts`:

```ts
import type { Session } from './types';

export interface FleetStats {
  total: number;
  busy: number;    // working | spawning
  waiting: number; // waiting_for_input
  errored: number; // errored | orphaned
}

export function deriveFleetStats(sessions: Session[]): FleetStats {
  const stats: FleetStats = { total: sessions.length, busy: 0, waiting: 0, errored: 0 };
  for (const s of sessions) {
    if (s.status === 'working' || s.status === 'spawning') stats.busy++;
    else if (s.status === 'waiting_for_input') stats.waiting++;
    else if (s.status === 'errored' || s.status === 'orphaned') stats.errored++;
  }
  return stats;
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/stats.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/stats.ts web/src/lib/stats.test.ts
git commit -m "feat(web): deriveFleetStats"
```

---

## Task 7: Frontend — `activity.ts`

**Files:**
- Create: `web/src/lib/activity.ts`
- Test: `web/src/lib/activity.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/activity.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { mergeEvents } from './activity';
import type { Session } from './types';

const sess = (id: string, events: Session['events']): Session =>
  ({ id, events } as Session);

describe('mergeEvents', () => {
  it('merges events across agents, newest first, tagged with the agent id', () => {
    const sessions = [
      sess('A-1', [
        { ts: '2026-06-03T10:00:00Z', type: 'spawned', detail: '' },
        { ts: '2026-06-03T10:05:00Z', type: 'tool', detail: 'edit' },
      ]),
      sess('B-2', [
        { ts: '2026-06-03T10:03:00Z', type: 'working', detail: '' },
      ]),
    ];
    const feed = mergeEvents(sessions);
    expect(feed.map((e) => [e.id, e.type])).toEqual([
      ['A-1', 'tool'],
      ['B-2', 'working'],
      ['A-1', 'spawned'],
    ]);
  });

  it('tolerates null event arrays and applies the limit', () => {
    const sessions = [
      sess('A-1', null),
      sess('B-2', [
        { ts: '2026-06-03T10:00:00Z', type: 'a', detail: '' },
        { ts: '2026-06-03T10:01:00Z', type: 'b', detail: '' },
        { ts: '2026-06-03T10:02:00Z', type: 'c', detail: '' },
      ]),
    ];
    expect(mergeEvents(sessions, 2).map((e) => e.type)).toEqual(['c', 'b']);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/activity.test.ts`
Expected: FAIL — cannot find module `./activity`.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/activity.ts`:

```ts
import type { Session } from './types';

export interface ActivityItem {
  id: string;     // agent id the event belongs to
  ts: string;
  type: string;
  detail: string;
}

// mergeEvents flattens every agent's event list into one feed, newest first,
// capped at `limit`. Each item is tagged with its agent id.
export function mergeEvents(sessions: Session[], limit = 50): ActivityItem[] {
  const items: ActivityItem[] = [];
  for (const s of sessions) {
    for (const e of s.events ?? []) {
      items.push({ id: s.id, ts: e.ts, type: e.type, detail: e.detail });
    }
  }
  items.sort((a, b) => new Date(b.ts).getTime() - new Date(a.ts).getTime());
  return items.slice(0, limit);
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/activity.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/activity.ts web/src/lib/activity.test.ts
git commit -m "feat(web): mergeEvents activity feed"
```

---

## Task 8: Frontend — `notify.ts` (waiting transition detector)

**Files:**
- Create: `web/src/lib/notify.ts`
- Test: `web/src/lib/notify.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/notify.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { waitingTransitions } from './notify';
import type { Session } from './types';

const sess = (id: string, status: Session['status']): Session =>
  ({ id, status } as Session);

describe('waitingTransitions', () => {
  it('returns agents that newly entered waiting_for_input', () => {
    const prev = [sess('A-1', 'working'), sess('B-2', 'waiting_for_input')];
    const next = [sess('A-1', 'waiting_for_input'), sess('B-2', 'waiting_for_input')];
    // A-1 just transitioned; B-2 was already waiting.
    expect(waitingTransitions(prev, next).map((s) => s.id)).toEqual(['A-1']);
  });

  it('treats a brand-new waiting agent as a transition', () => {
    const next = [sess('C-3', 'waiting_for_input')];
    expect(waitingTransitions([], next).map((s) => s.id)).toEqual(['C-3']);
  });

  it('returns nothing when no one is waiting', () => {
    const prev = [sess('A-1', 'working')];
    const next = [sess('A-1', 'idle')];
    expect(waitingTransitions(prev, next)).toEqual([]);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/notify.test.ts`
Expected: FAIL — cannot find module `./notify`.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/notify.ts`:

```ts
import type { Session } from './types';

// waitingTransitions returns agents that are waiting_for_input in `next` but
// were NOT waiting_for_input in `prev` (including brand-new agents). These are
// the only events we surface as notifications.
export function waitingTransitions(prev: Session[], next: Session[]): Session[] {
  const wasWaiting = new Set(
    prev.filter((s) => s.status === 'waiting_for_input').map((s) => s.id),
  );
  return next.filter(
    (s) => s.status === 'waiting_for_input' && !wasWaiting.has(s.id),
  );
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/notify.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/notify.ts web/src/lib/notify.test.ts
git commit -m "feat(web): waitingTransitions detector for notifications"
```

---

## Task 9: Frontend — add xterm.js dependency

**Files:**
- Modify: `web/package.json`

- [ ] **Step 1: Install xterm and the fit addon**

Run: `cd web && npm install @xterm/xterm@^5.5.0 @xterm/addon-fit@^0.10.0`
Expected: `package.json` `dependencies` now include `@xterm/xterm` and `@xterm/addon-fit`; `package-lock.json` updated.

- [ ] **Step 2: Verify the build still works**

Run: `cd web && npm run build`
Expected: build succeeds (no usage yet, just dependency resolution).

- [ ] **Step 3: Commit**

```bash
git add web/package.json web/package-lock.json
git commit -m "build(web): add xterm.js for the live terminal"
```

---

## Task 10: Frontend — `Terminal.tsx` (xterm wrapper)

**Files:**
- Create: `web/src/components/Terminal.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/Terminal.tsx`:

```tsx
import { useEffect, useRef } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { subscribeOutput } from '../lib/api';

// Terminal renders an agent's live tmux pane in color. tmux capture-pane returns
// the current visible screen (not a growing log), so each frame is a full
// snapshot: we reset() then write() the new frame.
export default function Terminal({ id }: { id: string }) {
  const hostRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

    const term = new XTerm({
      convertEol: true,
      fontSize: 12,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      theme: { background: '#0b0b0b', foreground: '#d6d6d6' },
      scrollback: 1000,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    const onResize = () => { try { fit.fit(); } catch { /* host detached */ } };
    window.addEventListener('resize', onResize);

    const unsub = subscribeOutput(id, (frame) => {
      term.reset();
      term.write(frame);
    });

    return () => {
      unsub();
      window.removeEventListener('resize', onResize);
      term.dispose();
    };
  }, [id]);

  return <div className="xterm-host" ref={hostRef} />;
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx astro check 2>/dev/null || npx tsc --noEmit`
Expected: no type errors referencing `Terminal.tsx`. (If `astro check` isn't configured, `tsc --noEmit` is the fallback. Either passing is acceptable.)

- [ ] **Step 3: Commit**

```bash
git add web/src/components/Terminal.tsx
git commit -m "feat(web): xterm.js Terminal wrapper fed by the output SSE stream"
```

---

## Task 11: Frontend — `MiniTerminal.tsx` (polled tile)

**Files:**
- Create: `web/src/components/MiniTerminal.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/MiniTerminal.tsx`:

```tsx
import { useEffect, useRef, useState } from 'react';
import { getOutput } from '../lib/api';

// MiniTerminal is a cheap glance at an agent's pane for the grid tiles: it polls
// the plain (uncolored) output endpoint and shows the last `lines` rows. Not a
// live feed — that's what the full Terminal is for.
export default function MiniTerminal({ id, lines = 8, intervalMs = 2000 }: {
  id: string;
  lines?: number;
  intervalMs?: number;
}) {
  const [text, setText] = useState('');
  const preRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    let alive = true;
    const poll = async () => {
      try {
        const o = await getOutput(id, lines);
        if (alive) setText(o.split('\n').slice(-lines).join('\n'));
      } catch { /* agent may have ended; SSE list will drop it */ }
    };
    poll();
    const t = setInterval(poll, intervalMs);
    return () => { alive = false; clearInterval(t); };
  }, [id, lines, intervalMs]);

  useEffect(() => {
    if (preRef.current) preRef.current.scrollTop = preRef.current.scrollHeight;
  }, [text]);

  return <pre className="mini-term" ref={preRef}>{text || '…'}</pre>;
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `MiniTerminal.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/MiniTerminal.tsx
git commit -m "feat(web): MiniTerminal polled tile for grid panes"
```

---

## Task 12: Frontend — `AttentionBar.tsx`

**Files:**
- Create: `web/src/components/AttentionBar.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/AttentionBar.tsx`:

```tsx
// AttentionBar is the always-visible top strip: connection state, a count of
// agents that need the user (clicking jumps to Overview), the notifications
// toggle, and the New-agent action.
export default function AttentionBar({
  connected, attentionCount, notifyEnabled, onToggleNotify, onNew, onJumpAttention,
}: {
  connected: boolean;
  attentionCount: number;
  notifyEnabled: boolean;
  onToggleNotify: () => void;
  onNew: () => void;
  onJumpAttention: () => void;
}) {
  return (
    <header className="topbar">
      <h1>agentctl</h1>
      <span className={connected ? 'conn ok' : 'conn down'}>
        {connected ? 'live' : 'reconnecting…'}
      </span>
      {attentionCount > 0 && (
        <button className="attn-pill" onClick={onJumpAttention}>
          ⚠ {attentionCount} need{attentionCount === 1 ? 's' : ''} you
        </button>
      )}
      <button className="notify-toggle" onClick={onToggleNotify} title="Browser notifications when an agent needs input">
        {notifyEnabled ? '🔔 on' : '🔕 off'}
      </button>
      <button className="new-btn" onClick={onNew}>+ New agent</button>
    </header>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `AttentionBar.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/AttentionBar.tsx
git commit -m "feat(web): AttentionBar top strip"
```

---

## Task 13: Frontend — `TabBar.tsx`

**Files:**
- Create: `web/src/components/TabBar.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/TabBar.tsx`:

```tsx
import type { Session } from '../lib/types';
import type { TabsState } from '../lib/tabs';
import BusyIdleBadge from './BusyIdleBadge';

// TabBar shows the two fixed tabs (Overview, Cockpit) plus one closeable tab per
// pinned agent. Unknown/ended agent ids are skipped (prune handles removal).
export default function TabBar({ state, sessions, onActivate, onClose }: {
  state: TabsState;
  sessions: Session[];
  onActivate: (id: string) => void;
  onClose: (id: string) => void;
}) {
  const byId = new Map(sessions.map((s) => [s.id, s]));
  const cls = (id: string) => `tab${state.active === id ? ' on' : ''}`;
  return (
    <nav className="tabbar">
      <button className={cls('overview')} onClick={() => onActivate('overview')}>Overview</button>
      <button className={cls('cockpit')} onClick={() => onActivate('cockpit')}>⊞ Cockpit</button>
      {state.pinned.map((id) => {
        const s = byId.get(id);
        if (!s) return null;
        return (
          <span key={id} className={cls(id)}>
            <button className="tab-label" onClick={() => onActivate(id)}>
              {id} <BusyIdleBadge status={s.status} />
            </button>
            <button className="tab-close" title="Close tab" onClick={() => onClose(id)}>✕</button>
          </span>
        );
      })}
    </nav>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `TabBar.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/TabBar.tsx
git commit -m "feat(web): TabBar with fixed + pinned agent tabs"
```

---

## Task 14: Frontend — `AttentionQueue.tsx`

**Files:**
- Create: `web/src/components/AttentionQueue.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/AttentionQueue.tsx`:

```tsx
import type { Session } from '../lib/types';
import { needsAttention } from '../lib/attention';
import BusyIdleBadge from './BusyIdleBadge';

// AttentionQueue surfaces agents blocked on the user or failed. Clicking a card
// pins + focuses that agent.
export default function AttentionQueue({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  const items = needsAttention(sessions);
  if (items.length === 0) {
    return <p className="muted attn-empty">Nothing needs you right now. ✅</p>;
  }
  return (
    <div className="attn-queue">
      {items.map((s) => (
        <button key={s.id} className="attn-card" onClick={() => onSelect(s.id)}>
          <div className="attn-card-head">
            <b>{s.id}</b> <BusyIdleBadge status={s.status} />
          </div>
          <div className="muted attn-card-sub">{s.subject || s.prompt || s.type || '—'}</div>
        </button>
      ))}
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `AttentionQueue.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/AttentionQueue.tsx
git commit -m "feat(web): AttentionQueue cards"
```

---

## Task 15: Frontend — `FleetStats.tsx`

**Files:**
- Create: `web/src/components/FleetStats.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/FleetStats.tsx`:

```tsx
import type { Session } from '../lib/types';
import { deriveFleetStats } from '../lib/stats';
import { groupSessions } from '../lib/group';

// FleetStats is the at-a-glance health summary: status counters plus a per-dir
// agent count.
export default function FleetStats({ sessions }: { sessions: Session[] }) {
  const stats = deriveFleetStats(sessions);
  const groups = groupSessions(sessions);
  return (
    <div className="fleet-stats">
      <div className="stat"><span className="stat-n">{stats.total}</span> total</div>
      <div className="stat busy"><span className="stat-n">{stats.busy}</span> busy</div>
      <div className="stat attention"><span className="stat-n">{stats.waiting}</span> waiting</div>
      <div className="stat error"><span className="stat-n">{stats.errored}</span> errored</div>
      <div className="stat-dirs">
        {groups.map((g) => (
          <span key={g.dir} className="stat-dir muted">{g.dir} ({g.sessions.length})</span>
        ))}
      </div>
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `FleetStats.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/FleetStats.tsx
git commit -m "feat(web): FleetStats summary"
```

---

## Task 16: Frontend — `AgentGrid.tsx`

**Files:**
- Create: `web/src/components/AgentGrid.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/AgentGrid.tsx`:

```tsx
import type { Session } from '../lib/types';
import { groupSessions } from '../lib/group';
import MiniTerminal from './MiniTerminal';
import BusyIdleBadge from './BusyIdleBadge';

// AgentGrid renders live thumbnail tiles for every agent, grouped by directory.
// Clicking a tile pins + focuses that agent. `lines` controls tile height; the
// Cockpit tab passes a larger value than the Overview mini-grid.
export default function AgentGrid({ sessions, onSelect, lines = 8 }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  lines?: number;
}) {
  if (sessions.length === 0) {
    return <p className="muted">No agents yet.</p>;
  }
  const groups = groupSessions(sessions);
  return (
    <div className="agent-grid-groups">
      {groups.map((g) => (
        <div key={g.dir} className="agent-grid-group">
          <div className="muted grid-group-head">{g.dir} ({g.sessions.length})</div>
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

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `AgentGrid.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/AgentGrid.tsx
git commit -m "feat(web): AgentGrid live thumbnail tiles, grouped by dir"
```

---

## Task 17: Frontend — `ActivityFeed.tsx`

**Files:**
- Create: `web/src/components/ActivityFeed.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/ActivityFeed.tsx`:

```tsx
import type { Session } from '../lib/types';
import { mergeEvents } from '../lib/activity';

// ActivityFeed is a merged, newest-first event stream across all agents.
export default function ActivityFeed({ sessions }: { sessions: Session[] }) {
  const items = mergeEvents(sessions);
  if (items.length === 0) return <p className="muted">No activity yet.</p>;
  return (
    <ul className="timeline activity-feed">
      {items.map((e, i) => (
        <li key={i}>
          <time>{e.ts ? new Date(e.ts).toLocaleTimeString() : ''}</time>{' '}
          <code className="muted">{e.id}</code>{' '}
          <b>{e.type}</b>{e.detail && <span className="muted"> — {e.detail}</span>}
        </li>
      ))}
    </ul>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `ActivityFeed.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/ActivityFeed.tsx
git commit -m "feat(web): ActivityFeed merged event stream"
```

---

## Task 18: Frontend — `QuickSpawn.tsx`

**Files:**
- Create: `web/src/components/QuickSpawn.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/QuickSpawn.tsx`:

```tsx
import { useState } from 'react';
import { spawn, ApiError } from '../lib/api';
import DirPicker from './DirPicker';

// QuickSpawn is the inline New-agent form on the Overview tab: a prompt plus a
// directory picker. Mirrors NewAgentModal's logic without the modal chrome.
export default function QuickSpawn({ onCreated }: { onCreated: (id: string) => void }) {
  const [prompt, setPrompt] = useState('');
  const [dir, setDir] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit() {
    setErr(null);
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
    if (!dir) { setErr('choose a directory to launch the agent from'); return; }
    setBusy(true);
    try {
      const s = await spawn({ prompt, cwd: dir });
      setPrompt('');
      onCreated(s.id);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : (e instanceof Error ? e.message : String(e)));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="quick-spawn">
      <textarea
        rows={3}
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        placeholder="What should a new agent do? (⌘/Ctrl+Enter to launch)"
        onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) submit(); }}
      />
      <DirPicker value={dir} onChange={setDir} />
      {err && <p className="warn">{err}</p>}
      <button disabled={busy || !dir} onClick={submit}>Launch agent</button>
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `QuickSpawn.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/QuickSpawn.tsx
git commit -m "feat(web): QuickSpawn inline new-agent form"
```

---

## Task 19: Frontend — `OverviewTab.tsx`

**Files:**
- Create: `web/src/components/OverviewTab.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/OverviewTab.tsx`:

```tsx
import type { Session } from '../lib/types';
import AttentionQueue from './AttentionQueue';
import FleetStats from './FleetStats';
import QuickSpawn from './QuickSpawn';
import AgentGrid from './AgentGrid';
import ActivityFeed from './ActivityFeed';

// OverviewTab composes the four home-screen sections: attention queue, fleet
// stats, quick spawn, the all-agents mini-grid, and the recent activity feed.
export default function OverviewTab({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  return (
    <div className="overview">
      <section className="card">
        <h3>Needs you</h3>
        <AttentionQueue sessions={sessions} onSelect={onSelect} />
      </section>
      <section className="card">
        <h3>Fleet</h3>
        <FleetStats sessions={sessions} />
      </section>
      <section className="card">
        <h3>Quick spawn</h3>
        <QuickSpawn onCreated={onSelect} />
      </section>
      <section className="card overview-grid">
        <h3>All agents</h3>
        <AgentGrid sessions={sessions} onSelect={onSelect} lines={6} />
      </section>
      <section className="card overview-activity">
        <h3>Recent activity</h3>
        <ActivityFeed sessions={sessions} />
      </section>
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `OverviewTab.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/OverviewTab.tsx
git commit -m "feat(web): OverviewTab composition"
```

---

## Task 20: Frontend — `CockpitTab.tsx`

**Files:**
- Create: `web/src/components/CockpitTab.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/CockpitTab.tsx`:

```tsx
import type { Session } from '../lib/types';
import AgentGrid from './AgentGrid';

// CockpitTab is the full-size live grid (taller tiles than the Overview
// mini-grid). Clicking a pane pins + focuses that agent.
export default function CockpitTab({ sessions, onSelect }: {
  sessions: Session[];
  onSelect: (id: string) => void;
}) {
  return (
    <div className="cockpit">
      <AgentGrid sessions={sessions} onSelect={onSelect} lines={14} />
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `CockpitTab.tsx`.

- [ ] **Step 3: Commit**

```bash
git add web/src/components/CockpitTab.tsx
git commit -m "feat(web): CockpitTab full-size live grid"
```

---

## Task 21: Frontend — rework `TerminateControls.tsx`

**Files:**
- Modify: `web/src/components/TerminateControls.tsx` (full rewrite)

- [ ] **Step 1: Rewrite the component against the real endpoints**

Replace the entire contents of `web/src/components/TerminateControls.tsx` with:

```tsx
import { useState } from 'react';
import type { Session } from '../lib/types';
import { terminate, removeWorktree, deleteSession, ApiError } from '../lib/api';

// TerminateControls drives the real teardown endpoints:
//   Terminate         -> POST /sessions/{id}/terminate         (stop the agent)
//   Remove worktree   -> POST /sessions/{id}/remove-worktree   (force on 409 guard)
//   Hard-delete record-> POST /sessions/{id}/delete {hard:true}
export default function TerminateControls({ session, onDone }: {
  session: Session;
  onDone: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [guard, setGuard] = useState<string | null>(null); // 409 message from remove-worktree

  async function doTerminate() {
    setBusy(true); setErr(null);
    try {
      await terminate(session.id);
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  }

  async function doRemoveWorktree(force: boolean) {
    setBusy(true); setErr(null);
    try {
      await removeWorktree(session.id, force);
      setGuard(null);
      onDone();
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setGuard(e.message);
      else setErr(e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  }

  async function doDelete() {
    setBusy(true); setErr(null);
    try {
      await deleteSession(session.id, true);
      onDone();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally { setBusy(false); }
  }

  if (guard) {
    return (
      <div className="terminate guard">
        <p className="warn">{guard}</p>
        <div className="actions">
          <button className="danger" disabled={busy} onClick={() => doRemoveWorktree(true)}>
            Force remove worktree + branch
          </button>
          <button disabled={busy} onClick={() => setGuard(null)}>Cancel</button>
        </div>
      </div>
    );
  }

  return (
    <div className="terminate">
      <div className="actions">
        <button className="danger" disabled={busy} onClick={doTerminate}>Terminate</button>
        {session.worktree && (
          <button disabled={busy} onClick={() => doRemoveWorktree(false)}>Remove worktree</button>
        )}
        <button disabled={busy} onClick={doDelete}>Delete record</button>
      </div>
      {err && <span className="warn"> {err}</span>}
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `TerminateControls.tsx`. (`AgentDetail.tsx` still imports the old `{ id }` prop — it is replaced by `AgentTab.tsx` in the next task and removed in Task 23, so a temporary type error there is expected until then. If `tsc` flags only `AgentDetail.tsx`, proceed.)

- [ ] **Step 3: Commit**

```bash
git add web/src/components/TerminateControls.tsx
git commit -m "fix(web): TerminateControls uses real terminate/remove-worktree/delete endpoints"
```

---

## Task 22: Frontend — `AgentTab.tsx`

**Files:**
- Create: `web/src/components/AgentTab.tsx`

- [ ] **Step 1: Write the component**

Create `web/src/components/AgentTab.tsx`:

```tsx
import { useState } from 'react';
import type { Session } from '../lib/types';
import { sendInput } from '../lib/api';
import Terminal from './Terminal';
import EventTimeline from './EventTimeline';
import TerminateControls from './TerminateControls';
import BusyIdleBadge from './BusyIdleBadge';

// AgentTab is the focused single-agent view: a live colored terminal, a send
// box, collapsible details + event timeline, and teardown controls.
export default function AgentTab({ session, onClosed }: {
  session: Session;
  onClosed: () => void;
}) {
  const [msg, setMsg] = useState('');
  const [sending, setSending] = useState(false);
  const [showDetails, setShowDetails] = useState(false);

  async function send() {
    if (!msg.trim()) return;
    setSending(true);
    try {
      await sendInput(session.id, msg);
      setMsg('');
    } catch { /* surfaced via list status / SSE */ } finally {
      setSending(false);
    }
  }

  return (
    <div className="agent-tab">
      <div className="agent-tab-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} /></h2>
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || session.repo || '—'}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      <Terminal id={session.id} />

      <section className="sendbox">
        <input
          value={msg}
          onChange={(e) => setMsg(e.target.value)}
          placeholder="Send a message to this agent…"
          onKeyDown={(e) => { if (e.key === 'Enter') send(); }}
        />
        <button disabled={sending} onClick={send}>Send</button>
      </section>

      <button className="details-toggle" onClick={() => setShowDetails((v) => !v)}>
        {showDetails ? '▾ Hide details' : '▸ Details & history'}
      </button>
      {showDetails && (
        <section className="agent-details">
          {session.prompt && (
            <p className="muted" style={{ whiteSpace: 'pre-wrap' }}>{session.prompt}</p>
          )}
          <EventTimeline events={session.events} />
          {session.worktree && (
            <p className="muted">Attach in a terminal: <code>agentctl attach {session.id}</code></p>
          )}
        </section>
      )}
    </div>
  );
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd web && npx tsc --noEmit`
Expected: no type errors referencing `AgentTab.tsx`. (As in Task 21, `AgentDetail.tsx` may still error; it is removed in Task 23.)

- [ ] **Step 3: Commit**

```bash
git add web/src/components/AgentTab.tsx
git commit -m "feat(web): AgentTab focused single-agent view with live terminal"
```

---

## Task 23: Frontend — `Dashboard.tsx` shell + notifications, retire old components

**Files:**
- Modify: `web/src/components/Dashboard.tsx` (full rewrite)
- Delete: `web/src/components/AgentDetail.tsx`, `web/src/components/AgentList.tsx`

- [ ] **Step 1: Rewrite `Dashboard.tsx` as the tabbed shell**

Replace the entire contents of `web/src/components/Dashboard.tsx` with:

```tsx
import { useEffect, useReducer, useRef, useState } from 'react';
import type { Session } from '../lib/types';
import { listSessions, subscribeSessions } from '../lib/api';
import { tabsReducer, initialTabs, type TabsState } from '../lib/tabs';
import { waitingTransitions } from '../lib/notify';
import AttentionBar from './AttentionBar';
import TabBar from './TabBar';
import OverviewTab from './OverviewTab';
import CockpitTab from './CockpitTab';
import AgentTab from './AgentTab';
import NewAgentModal from './NewAgentModal';

const TABS_KEY = 'agentctl.tabs';

function loadTabs(): TabsState {
  try {
    const raw = localStorage.getItem(TABS_KEY);
    if (raw) return JSON.parse(raw) as TabsState;
  } catch { /* corrupt / unavailable storage */ }
  return initialTabs;
}

export default function Dashboard() {
  const [sessions, setSessions] = useState<Session[]>([]);
  const [connected, setConnected] = useState(false);
  const [showCreate, setShowCreate] = useState(false);
  const [notifyEnabled, setNotifyEnabled] = useState(false);
  const [tabs, dispatch] = useReducer(tabsReducer, undefined, loadTabs);
  const prevSessions = useRef<Session[]>([]);

  // Live session list over SSE.
  useEffect(() => {
    listSessions().then(setSessions).catch(() => { /* SSE will populate */ });
    const unsub = subscribeSessions(setSessions, () => setConnected(false), () => setConnected(true));
    return unsub;
  }, []);

  // Persist tabs; prune pins for agents that ended.
  useEffect(() => { try { localStorage.setItem(TABS_KEY, JSON.stringify(tabs)); } catch { /* ignore */ } }, [tabs]);
  useEffect(() => {
    dispatch({ kind: 'prune', alive: sessions.map((s) => s.id) });
  }, [sessions]);

  // Notify when an agent newly needs input, but only while the tab is hidden.
  useEffect(() => {
    const prev = prevSessions.current;
    prevSessions.current = sessions;
    if (!notifyEnabled || Notification.permission !== 'granted') return;
    if (!document.hidden) return;
    for (const s of waitingTransitions(prev, sessions)) {
      const n = new Notification(`${s.id} needs your input`, {
        body: s.subject || s.prompt || 'Waiting for input',
        tag: s.id,
      });
      n.onclick = () => { window.focus(); dispatch({ kind: 'open', id: s.id }); n.close(); };
    }
  }, [sessions, notifyEnabled]);

  async function toggleNotify() {
    if (notifyEnabled) { setNotifyEnabled(false); return; }
    const perm = Notification.permission === 'granted'
      ? 'granted'
      : await Notification.requestPermission();
    setNotifyEnabled(perm === 'granted');
  }

  const attentionCount = sessions.filter(
    (s) => s.status === 'waiting_for_input' || s.status === 'errored' || s.status === 'orphaned',
  ).length;
  const select = (id: string) => dispatch({ kind: 'open', id });
  const activeSession = sessions.find((s) => s.id === tabs.active) ?? null;

  return (
    <div className="layout">
      <AttentionBar
        connected={connected}
        attentionCount={attentionCount}
        notifyEnabled={notifyEnabled}
        onToggleNotify={toggleNotify}
        onNew={() => setShowCreate(true)}
        onJumpAttention={() => dispatch({ kind: 'activate', id: 'overview' })}
      />
      <TabBar
        state={tabs}
        sessions={sessions}
        onActivate={(id) => dispatch({ kind: 'activate', id })}
        onClose={(id) => dispatch({ kind: 'close', id })}
      />
      <main className="tab-content">
        {tabs.active === 'overview' && <OverviewTab sessions={sessions} onSelect={select} />}
        {tabs.active === 'cockpit' && <CockpitTab sessions={sessions} onSelect={select} />}
        {activeSession && <AgentTab session={activeSession} onClosed={() => dispatch({ kind: 'close', id: activeSession.id })} />}
        {tabs.active !== 'overview' && tabs.active !== 'cockpit' && !activeSession && (
          <div className="detail empty">That agent has ended.</div>
        )}
      </main>
      {showCreate && (
        <NewAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(id) => { setShowCreate(false); dispatch({ kind: 'open', id }); }}
        />
      )}
    </div>
  );
}
```

- [ ] **Step 2: Delete the retired components**

Run: `git rm web/src/components/AgentDetail.tsx web/src/components/AgentList.tsx`
Expected: both files removed (their roles are replaced by `AgentTab` + `AgentGrid`/`TabBar`).

- [ ] **Step 3: Verify the whole frontend type-checks and builds**

Run: `cd web && npx tsc --noEmit && npm run build`
Expected: no type errors anywhere; build succeeds.

- [ ] **Step 4: Commit**

```bash
git add web/src/components/Dashboard.tsx
git commit -m "feat(web): tabbed mission-control shell with notifications; retire list/detail"
```

---

## Task 24: Frontend — styles

**Files:**
- Modify: `web/src/styles/app.css` (append)

- [ ] **Step 1: Append the new styles**

Add the following to the END of `web/src/styles/app.css` (keep all existing rules — `AttentionBar` reuses `.topbar`, `.conn`, `BusyIdleBadge` reuses `.badge`, `TerminateControls` reuses `.terminate`/`.danger`, `EventTimeline` reuses `.timeline`):

```css
/* ── Mission-control shell ── */
.tabbar { display: flex; gap: .25rem; padding: 0 1rem; border-bottom: 1px solid #8884; overflow-x: auto; }
.tabbar .tab { display: inline-flex; align-items: center; gap: .3rem; background: transparent; border: none; border-bottom: 2px solid transparent; padding: .5rem .7rem; cursor: pointer; color: inherit; font: inherit; white-space: nowrap; }
.tabbar .tab.on { border-bottom-color: #2f81f7; font-weight: 600; }
.tabbar .tab-label { background: none; border: none; color: inherit; font: inherit; cursor: pointer; display: inline-flex; align-items: center; gap: .3rem; }
.tabbar .tab-close { background: none; border: none; color: var(--idle); cursor: pointer; padding: 0 .2rem; }
.tabbar .tab-close:hover { color: var(--error); }
.tab-content { flex: 1; overflow: auto; padding: 1rem; }
.attn-pill { background: var(--attention); color: #fff; border: none; border-radius: 1rem; padding: .15rem .7rem; cursor: pointer; }
.notify-toggle { background: #8882; border: none; border-radius: 1rem; padding: .15rem .7rem; cursor: pointer; color: inherit; }
.topbar .new-btn { margin-left: 0; }

/* ── Overview ── */
.overview { display: grid; grid-template-columns: 1fr 1fr; gap: 1rem; align-items: start; }
.overview .card { border: 1px solid #8883; border-radius: .5rem; padding: .8rem; }
.overview .card h3 { margin: 0 0 .6rem; font-size: .95rem; }
.overview .overview-grid, .overview .overview-activity { grid-column: 1 / -1; }
.attn-queue { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: .5rem; }
.attn-card { text-align: left; background: #b0880018; border: 1px solid #b0880055; border-radius: .4rem; padding: .5rem .6rem; cursor: pointer; color: inherit; }
.attn-card:hover { background: #b0880033; }
.attn-card-sub { font-size: .8rem; margin-top: .2rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.attn-empty { padding: .5rem 0; }
.fleet-stats { display: flex; flex-wrap: wrap; gap: .8rem; align-items: baseline; }
.fleet-stats .stat { font-size: .85rem; color: var(--idle); }
.fleet-stats .stat-n { font-size: 1.3rem; font-weight: 700; color: CanvasText; margin-right: .2rem; }
.fleet-stats .stat.busy .stat-n { color: var(--busy); }
.fleet-stats .stat.attention .stat-n { color: var(--attention); }
.fleet-stats .stat.error .stat-n { color: var(--error); }
.fleet-stats .stat-dirs { display: flex; flex-wrap: wrap; gap: .5rem; width: 100%; font-size: .8rem; }
.quick-spawn { display: flex; flex-direction: column; gap: .5rem; }
.quick-spawn textarea { padding: .5rem; font: inherit; resize: vertical; }

/* ── Agent grid / cockpit ── */
.agent-grid-group { margin-bottom: 1rem; }
.grid-group-head { margin: .3rem 0; font-size: .8rem; }
.agent-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: .6rem; }
.grid-tile { text-align: left; padding: 0; border: 1px solid #8884; border-radius: .4rem; overflow: hidden; cursor: pointer; background: transparent; color: inherit; }
.grid-tile:hover { border-color: #2f81f7; }
.tile-head { display: flex; align-items: center; gap: .4rem; padding: .35rem .5rem; font-size: .85rem; background: #8881; }
.mini-term { margin: 0; background: #0b0b0b; color: #8fd98f; padding: .4rem; font-size: .72rem; line-height: 1.25; white-space: pre-wrap; overflow: hidden; }

/* ── Agent tab ── */
.agent-tab { display: flex; flex-direction: column; gap: .8rem; height: 100%; }
.agent-tab-head { display: flex; flex-direction: column; gap: .4rem; }
.agent-tab-head h2 { margin: 0; font-size: 1.1rem; }
.xterm-host { height: 420px; background: #0b0b0b; border-radius: .4rem; padding: .4rem; }
.details-toggle { align-self: flex-start; background: none; border: none; color: var(--idle); cursor: pointer; font: inherit; }
.agent-details { display: flex; flex-direction: column; gap: .5rem; }
```

- [ ] **Step 2: Verify the build still works**

Run: `cd web && npm run build`
Expected: build succeeds.

- [ ] **Step 3: Commit**

```bash
git add web/src/styles/app.css
git commit -m "style(web): mission-control shell, overview, grid, and agent-tab styles"
```

---

## Task 25: Full verification + final commit

**Files:** none (verification only)

- [ ] **Step 1: Run the full Go test suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 2: Run the full web test suite**

Run: `cd web && npx vitest run`
Expected: all tests PASS (api, tabs, attention, stats, activity, notify, status, group).

- [ ] **Step 3: Type-check and build the web bundle**

Run: `cd web && npx tsc --noEmit && npm run build`
Expected: no type errors; `dist/` rebuilt.

- [ ] **Step 4: Manual smoke test (the part automated tests don't cover)**

Start the daemon and open the web UI (use the project's existing run path — e.g. `agentctl serve` / `agentctl gui`, or the documented dev command). Verify:
- Overview shows attention queue / fleet stats / quick spawn / mini-grid / activity feed.
- Spawning from Quick spawn pins a new agent tab.
- The agent tab's xterm terminal shows **colored** live output and updates ~1s.
- Cockpit tab shows the live grid; clicking a tile pins that agent.
- Terminate, Remove worktree (with force on a dirty/unpushed guard), and Delete record all work and remove the tab.
- Toggling 🔔, then backgrounding the tab while an agent asks for input, fires a desktop notification that focuses + opens that agent on click.
- Reloading the page restores the open tabs.

- [ ] **Step 5: Final commit (if the manual smoke surfaced any tweaks)**

```bash
git add -A
git commit -m "test(web): verify mission-control end-to-end"
```

---

## Self-Review (completed by plan author)

**Spec coverage:**
- Tabbed shell (Overview/Cockpit/pinned) → Tasks 4, 13, 23. ✓
- Multi-pane cockpit → Tasks 16, 20. ✓
- Live colored terminal streaming → Tasks 1, 2, 3 (`subscribeOutput`), 9, 10. ✓
- Attention queue → Tasks 5, 14, 19. ✓
- Notifications (waiting_for_input, hidden-gated) → Tasks 8, 23. ✓
- Fleet stats / activity feed / quick spawn / mini-grid → Tasks 6, 7, 15, 17, 18, 19. ✓
- Fix broken Terminate → Tasks 3, 21. ✓
- Two-tier output (polled tiles vs SSE focus) → Tasks 11 (MiniTerminal poll), 10 (Terminal SSE). ✓
- Tab persistence/restore → Task 23 (`localStorage` + `prune`). ✓
- Testing: lib unit tests + Go handler test → Tasks 2–8; component verification via build → throughout. ✓ (Deviation from spec's jsdom component tests is documented in the header note.)

**Placeholder scan:** No TBD/TODO/"handle edge cases"; every code step shows full code. ✓

**Type consistency:** `TabsState`/`TabsAction`/`tabsReducer`/`initialTabs` consistent across Tasks 4, 13, 23. `subscribeOutput(id, onFrame, onError?)` defined in Task 3, used in Task 10. `terminate`/`removeWorktree`/`deleteSession` signatures consistent across Tasks 3 and 21. `needsAttention`/`deriveFleetStats`/`mergeEvents`/`waitingTransitions` signatures consistent between their lib tasks and consuming components. `TerminateControls` prop changed from `{id}` to `{session}` — old consumers (`AgentDetail`) deleted in Task 23. ✓
