# Web Interactive Terminal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the web agent tab's read-only snapshot terminal + send box with a genuinely interactive terminal — a real `tmux attach` bridged to xterm.js over a WebSocket (keystrokes as binary frames, `{cols,rows}` resize as text frames), and remove the now-dead SSE snapshot path.

**Architecture:** A new daemon WebSocket endpoint `GET /sessions/{id}/attach` runs `tmux attach-session` in a PTY (creack/pty) and pumps bytes both ways over the socket (coder/websocket). The browser uses xterm.js wired directly to that socket. The session's tmux `window-size` is set to `latest` so the most recently active client drives sizing. The previously-built 1s-snapshot path (`/output/stream`, `OutputANSI`, `Terminal.tsx`, `subscribeOutput`) is deleted; the cheap plain `/output` poll (cockpit mini-tiles) and `/input` (CLI/MCP/TUI) stay.

**Tech Stack:** Go (chi, `github.com/creack/pty@v1.1.24`, `github.com/coder/websocket@v1.8.13` — both already in the local module cache), Astro + React 19 + TypeScript + xterm.js, Vitest.

**Testing approach note:** Repo convention — pure logic is unit-tested; React components and live tmux/PTY paths are build-verified + manually smoke-tested (no `@testing-library`). `parseResize`/`attachURL`/`resizeMessage` get unit tests; the PTY↔WS bridge and live attach are manual.

---

## File Structure

**Backend:**
- Delete `internal/daemon/output_stream.go` + `internal/daemon/output_stream_test.go`.
- Modify `internal/daemon/lifecycle_routes.go` — drop the `/output/stream` route, add the `/attach` route.
- Modify `internal/daemon/api.go`, `internal/daemon/lifecycle_adapter.go`, `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`, `internal/daemon/lifecycle_routes_test.go` — remove `OutputANSI`.
- Create `internal/daemon/attach.go` — `handleAttach` + `parseResize`.
- Create `internal/daemon/attach_test.go` — `parseResize` + 404 tests.
- Modify `go.mod` / `go.sum` — add the two deps.

**Frontend:**
- Create `web/src/lib/attach.ts` + `web/src/lib/attach.test.ts` — `attachURL`, `resizeMessage`.
- Create `web/src/components/AttachTerminal.tsx`.
- Delete `web/src/components/Terminal.tsx`.
- Modify `web/src/lib/api.ts` — remove `subscribeOutput`.
- Modify `web/src/components/AgentTab.tsx` — use `AttachTerminal`, delete the send box.

---

## Task 1: Remove the superseded snapshot path (backend)

**Files:**
- Delete: `internal/daemon/output_stream.go`, `internal/daemon/output_stream_test.go`
- Modify: `internal/daemon/lifecycle_routes.go:26`, `internal/daemon/api.go:128`, `internal/daemon/lifecycle_adapter.go:94-96`, `internal/lifecycle/lifecycle.go` (OutputANSI), `internal/lifecycle/lifecycle_test.go` (TestOutputANSICapturesWithEscapes), `internal/daemon/lifecycle_routes_test.go:119` (fakeLife.OutputANSI)

> Note: run only `go test ./internal/daemon/ ./internal/lifecycle/` — `go test ./...` hangs on unrelated packages in this sandbox.

- [ ] **Step 1: Delete the SSE output-stream handler + its test**

```bash
git rm internal/daemon/output_stream.go internal/daemon/output_stream_test.go
```

- [ ] **Step 2: Unregister the route**

In `internal/daemon/lifecycle_routes.go`, delete the line:

```go
	r.Get("/sessions/{id}/output/stream", s.handleOutputStream)
```

(Keep the preceding `r.Get("/sessions/{id}/output", s.handleOutput)` line.)

- [ ] **Step 3: Remove `OutputANSI` from the interface**

In `internal/daemon/api.go`, delete the line in the `Lifecycle interface` block:

```go
	OutputANSI(ctx context.Context, tmuxSession string, lines int) (string, error)
```

- [ ] **Step 4: Remove `OutputANSI` from the adapter**

In `internal/daemon/lifecycle_adapter.go`, delete the method:

```go
func (a *lifecycleAdapter) OutputANSI(ctx context.Context, tmuxSession string, lines int) (string, error) {
	return a.lc.OutputANSI(ctx, tmuxSession, lines)
}
```

- [ ] **Step 5: Remove `OutputANSI` from the lifecycle implementation**

In `internal/lifecycle/lifecycle.go`, delete the `OutputANSI` method (the doc comment beginning `// OutputANSI is like Output but preserves ANSI escape sequences` plus the function). Leave `Output` intact.

- [ ] **Step 6: Remove the `OutputANSI` lifecycle test**

In `internal/lifecycle/lifecycle_test.go`, delete the entire `func TestOutputANSICapturesWithEscapes(t *testing.T) { … }`.

- [ ] **Step 7: Remove the `fakeLife.OutputANSI` stub**

In `internal/daemon/lifecycle_routes_test.go`, delete:

```go
func (f *fakeLife) OutputANSI(_ context.Context, s string, n int) (string, error) {
	return f.output, nil
}
```

- [ ] **Step 8: Verify build + tests are green**

Run: `go build ./... && go test ./internal/daemon/ ./internal/lifecycle/`
Expected: build succeeds; all tests PASS (no references to `OutputANSI` or `handleOutputStream` remain — `grep -rn "OutputANSI\|handleOutputStream\|output/stream" internal/` returns nothing).

- [ ] **Step 9: Commit**

```bash
git add -A
git commit -m "refactor(daemon): remove SSE snapshot path (OutputANSI, /output/stream)"
```

---

## Task 2: WebSocket attach endpoint (backend)

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/daemon/attach.go`
- Create: `internal/daemon/attach_test.go`
- Modify: `internal/daemon/lifecycle_routes.go` (register route)

- [ ] **Step 1: Add the dependencies**

Run (both are already in the local module cache, so this works offline):

```bash
go get github.com/creack/pty@v1.1.24 github.com/coder/websocket@v1.8.13
```

Expected: `go.mod` gains both `require`s; `go.sum` updated. If this fails with a network error, STOP and report — the build host can't reach the Go proxy and the user must run it.

- [ ] **Step 2: Write the failing tests**

Create `internal/daemon/attach_test.go`:

```go
package daemon

import (
	"net/http"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestParseResize(t *testing.T) {
	c, r, ok := parseResize([]byte(`{"cols":120,"rows":40}`))
	require.True(t, ok)
	require.EqualValues(t, 120, c)
	require.EqualValues(t, 40, r)

	_, _, ok = parseResize([]byte(`{"cols":0,"rows":40}`))
	require.False(t, ok, "zero cols is invalid")

	_, _, ok = parseResize([]byte(`{"rows":40}`))
	require.False(t, ok, "missing cols defaults to 0 → invalid")

	_, _, ok = parseResize([]byte(`not json`))
	require.False(t, ok, "malformed JSON is invalid")
}

func TestAttachUnknownSessionIs404(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	// A plain GET (no WebSocket upgrade headers) must 404 before any upgrade.
	resp, err := http.Get(ts.URL + "/sessions/nope/attach")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAttachFoundSessionDoesNotFastReject(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(nil, &store.Session{ID: "A-1", TmuxSession: "A-1"})
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()
	// A plain GET on an existing session reaches websocket.Accept, which rejects a
	// non-upgrade request with 400 (NOT 404). This proves the session lookup passed.
	resp, err := http.Get(ts.URL + "/sessions/A-1/attach")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusNotFound, resp.StatusCode)
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestParseResize -v`
Expected: FAIL — `parseResize` undefined (compile error). (`TestAttach*` also won't compile yet — that's fine; they compile once the handler exists.)

- [ ] **Step 4: Write the handler**

Create `internal/daemon/attach.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/store"
)

// resizeMsg is the JSON body of a client text control frame.
type resizeMsg struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// parseResize decodes a client text frame into terminal dimensions. ok is false
// for malformed JSON or non-positive dimensions.
func parseResize(data []byte) (cols, rows uint16, ok bool) {
	var m resizeMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, 0, false
	}
	if m.Cols == 0 || m.Rows == 0 {
		return 0, 0, false
	}
	return m.Cols, m.Rows, true
}

// handleAttach bridges an interactive `tmux attach` to the browser over a
// WebSocket. A PTY runs the attach; its output streams to the client as binary
// frames; client binary frames are keystrokes written to the PTY; client text
// frames are {cols,rows} resize controls. Closing the socket detaches THIS
// client only — the agent's tmux session keeps running.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
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

	// The most recently active client drives the window size (web vs TUI vs
	// terminal). Best-effort; failure is non-fatal.
	_ = exec.Command("tmux", "set-option", "-t", sess.TmuxSession, "window-size", "latest").Run()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept already wrote the HTTP error response
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20) // 1 MiB — tolerate large pastes

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, "tmux", "attach-session", "-t", sess.TmuxSession)
	ptyFile, err := pty.Start(cmd)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "pty start failed")
		return
	}
	defer func() { _ = ptyFile.Close() }()

	// PTY → client.
	go func() {
		defer cancel()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := ptyFile.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// client → PTY: binary = keystrokes, text = resize control.
	for {
		typ, data, rerr := conn.Read(ctx)
		if rerr != nil {
			return
		}
		switch typ {
		case websocket.MessageText:
			if cols, rows, ok := parseResize(data); ok {
				_ = pty.Setsize(ptyFile, &pty.Winsize{Cols: cols, Rows: rows})
			}
		case websocket.MessageBinary:
			if _, werr := ptyFile.Write(data); werr != nil {
				return
			}
		}
	}
}
```

- [ ] **Step 5: Register the route**

In `internal/daemon/lifecycle_routes.go`, directly after the `r.Get("/sessions/{id}/output", s.handleOutput)` line, add:

```go
	r.Get("/sessions/{id}/attach", s.handleAttach)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestParseResize -v && go test ./internal/daemon/ -run TestAttach -v`
Expected: PASS (`TestParseResize`, `TestAttachUnknownSessionIs404`, `TestAttachFoundSessionDoesNotFastReject`).

- [ ] **Step 7: Full build + daemon/lifecycle tests**

Run: `go build ./... && go vet ./internal/daemon/ && go test ./internal/daemon/ ./internal/lifecycle/`
Expected: build OK, vet clean, all tests PASS.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "feat(daemon): interactive tmux attach over WebSocket (PTY bridge)"
```

---

## Task 3: Frontend `attach.ts` helpers

**Files:**
- Create: `web/src/lib/attach.ts`
- Test: `web/src/lib/attach.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/attach.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { attachURL, resizeMessage } from './attach';

describe('attachURL', () => {
  it('uses ws:// for an http page', () => {
    expect(attachURL({ protocol: 'http:', host: 'localhost:8765' }, 'A-1'))
      .toBe('ws://localhost:8765/sessions/A-1/attach');
  });
  it('uses wss:// for an https page', () => {
    expect(attachURL({ protocol: 'https:', host: 'host:443' }, 'A-1'))
      .toBe('wss://host:443/sessions/A-1/attach');
  });
  it('url-encodes the id', () => {
    expect(attachURL({ protocol: 'http:', host: 'h' }, 'a/b'))
      .toBe('ws://h/sessions/a%2Fb/attach');
  });
});

describe('resizeMessage', () => {
  it('serializes cols/rows as JSON', () => {
    expect(resizeMessage(120, 40)).toBe('{"cols":120,"rows":40}');
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/attach.test.ts`
Expected: FAIL — cannot find module `./attach`.

- [ ] **Step 3: Write the implementation**

Create `web/src/lib/attach.ts`:

```ts
// attachURL builds the WebSocket URL for an agent's interactive attach endpoint,
// matching the page's scheme (ws for http, wss for https).
export function attachURL(loc: { protocol: string; host: string }, id: string): string {
  const scheme = loc.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${loc.host}/sessions/${encodeURIComponent(id)}/attach`;
}

// resizeMessage is the text control frame announcing terminal dimensions.
export function resizeMessage(cols: number, rows: number): string {
  return JSON.stringify({ cols, rows });
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd web && npx vitest run src/lib/attach.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/attach.ts web/src/lib/attach.test.ts
git commit -m "feat(web): attachURL + resizeMessage helpers"
```

---

## Task 4: Interactive `AttachTerminal`, swap into `AgentTab`, remove snapshot frontend

**Files:**
- Create: `web/src/components/AttachTerminal.tsx`
- Delete: `web/src/components/Terminal.tsx`
- Modify: `web/src/lib/api.ts` (remove `subscribeOutput`)
- Modify: `web/src/components/AgentTab.tsx`

- [ ] **Step 1: Create `AttachTerminal.tsx`**

Create `web/src/components/AttachTerminal.tsx`:

```tsx
import { useEffect, useRef, useState } from 'react';
import { Terminal as XTerm } from '@xterm/xterm';
import { FitAddon } from '@xterm/addon-fit';
import '@xterm/xterm/css/xterm.css';
import { attachURL, resizeMessage } from '../lib/attach';

// AttachTerminal is a fully interactive terminal bridged to a real `tmux attach`
// over a WebSocket: keystrokes (binary frames) go to the agent, PTY output
// (binary frames) is rendered, and fit/resize is sent as a text control frame.
export default function AttachTerminal({ id }: { id: string }) {
  const hostRef = useRef<HTMLDivElement>(null);
  const [closed, setClosed] = useState(false);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    setClosed(false);

    const term = new XTerm({
      fontSize: 12,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
      theme: { background: '#0b0b0b', foreground: '#d6d6d6' },
      scrollback: 5000,
      cursorBlink: true,
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    const ws = new WebSocket(attachURL(window.location, id));
    ws.binaryType = 'arraybuffer';

    const sendResize = () => {
      try { fit.fit(); } catch { /* host detached */ }
      if (ws.readyState === WebSocket.OPEN) ws.send(resizeMessage(term.cols, term.rows));
    };

    ws.onopen = () => { sendResize(); term.focus(); };
    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) term.write(new Uint8Array(e.data));
    };
    ws.onclose = () => setClosed(true);
    ws.onerror = () => setClosed(true);

    const dataSub = term.onData((d) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(d));
    });

    const ro = new ResizeObserver(() => sendResize());
    ro.observe(host);

    const onClick = () => term.focus();
    host.addEventListener('click', onClick);

    return () => {
      host.removeEventListener('click', onClick);
      ro.disconnect();
      dataSub.dispose();
      ws.close();
      term.dispose();
    };
  }, [id]);

  return (
    <div className="xterm-wrap">
      {closed && (
        <div style={{ color: '#cf222e', fontSize: '.75rem', padding: '.15rem .4rem' }}>
          disconnected — the agent may have ended
        </div>
      )}
      <div className="xterm-host" ref={hostRef} tabIndex={0} />
    </div>
  );
}
```

- [ ] **Step 2: Remove `subscribeOutput` from the API client**

In `web/src/lib/api.ts`, delete the `subscribeOutput` function (the doc comment starting `// subscribeOutput opens an SSE connection…` through the end of that function). Leave `getOutput`, `subscribeSessions`, and everything else intact.

- [ ] **Step 3: Delete the old snapshot `Terminal.tsx`**

```bash
git rm web/src/components/Terminal.tsx
```

- [ ] **Step 4: Rewrite `AgentTab.tsx` (use AttachTerminal, drop the send box)**

Replace the ENTIRE contents of `web/src/components/AgentTab.tsx` with:

```tsx
import { useState } from 'react';
import type { Session } from '../lib/types';
import AttachTerminal from './AttachTerminal';
import EventTimeline from './EventTimeline';
import TerminateControls from './TerminateControls';
import BusyIdleBadge from './BusyIdleBadge';

// AgentTab is the focused single-agent view: a fully interactive terminal
// (real tmux attach), collapsible details + event timeline, and teardown controls.
export default function AgentTab({ session, onClosed }: {
  session: Session;
  onClosed: () => void;
}) {
  const [showDetails, setShowDetails] = useState(false);

  return (
    <div className="agent-tab">
      <div className="agent-tab-head">
        <h2>{session.id} <BusyIdleBadge status={session.status} /></h2>
        <code className="muted">
          type: {session.type || 'classifying…'} · dir: {session.workdir || session.repo || '—'}
        </code>
        <TerminateControls session={session} onDone={onClosed} />
      </div>

      <AttachTerminal id={session.id} />

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

- [ ] **Step 5: Verify type-check + build**

Run: `cd web && npx tsc --noEmit && npm run build`
Expected: no type errors (nothing imports `Terminal` or `subscribeOutput` anymore — confirm with `grep -rn "subscribeOutput\|from './Terminal'" web/src` → no matches); build succeeds.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "feat(web): interactive AttachTerminal in AgentTab; drop send box + snapshot Terminal"
```

---

## Task 5: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Backend**

Run: `go build ./... && go vet ./internal/daemon/ && go test ./internal/daemon/ ./internal/lifecycle/`
Expected: build OK, vet clean, tests PASS.

- [ ] **Step 2: Frontend**

Run: `cd web && npx tsc --noEmit && npm run build && npx vitest run`
Expected: tsc clean, build OK, all web tests PASS (existing suite + new `attach` tests; the removed `dirpath`/others still pass).

- [ ] **Step 3: Dead-reference sweep**

Run: `grep -rn "OutputANSI\|handleOutputStream\|output/stream\|subscribeOutput" internal/ web/src`
Expected: no matches.

- [ ] **Step 4: Manual smoke (not covered by automated tests)**

Rebuild + restart the daemon (binary embeds `web/dist`; run the install/`make release` path). Open the web UI, select a running agent:
- The agent tab shows a live terminal with **no separate send field**.
- Click it and type — keystrokes reach claude; ctrl-C, arrows, and claude's TUI render correctly with no perceptible lag.
- Resize the browser/pane — the terminal reflows (and, with `window-size latest`, a concurrently-attached TUI follows the active client).
- Closing the tab/agent shows the "disconnected" indicator; other attached clients (TUI) keep running.
- Cockpit/Overview mini-tiles still show their read-only polled output.

---

## Self-Review (completed by plan author)

**Spec coverage:**
- Interactive tmux attach over WS (PTY, binary keystrokes, text resize, `window-size latest`, 404-before-upgrade, kill-on-close) → Task 2. ✓
- Frontend interactive terminal replacing snapshot + send box → Task 4 (AttachTerminal + AgentTab rewrite). ✓
- `attachURL`/`resizeMessage` pure helpers + tests → Task 3. ✓
- Remove snapshot path (Terminal.tsx, subscribeOutput, /output/stream, OutputANSI) → Tasks 1 & 4. ✓
- Keep plain `/output` poll + `/input` → untouched (verified: Task 1 only removes the stream route/ANSI). ✓
- New deps creack/pty + coder/websocket → Task 2 Step 1. ✓
- Testing (parseResize, 404, attachURL, resizeMessage; manual bridge) → Tasks 2, 3, 5. ✓

**Placeholder scan:** No TBD/TODO/vague steps; every code step shows full code. ✓

**Type consistency:** `parseResize(data []byte) (cols, rows uint16, ok bool)` used in handler + tests. `pty.Winsize{Cols, Rows}` matches `parseResize` outputs. `attachURL(loc {protocol,host}, id)` + `resizeMessage(cols,rows)` match `AttachTerminal`'s usage (`window.location`, `term.cols/term.rows`). `AttachTerminal` default-exports a component taking `{ id }`; `AgentTab` imports it that way. Removal sites (interface line, adapter method, lifecycle method, two tests, route line) are each named with their location. ✓
