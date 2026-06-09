# Web interactive terminal (real tmux attach over WebSocket)

**Date:** 2026-06-03
**Status:** Approved (brainstorm) — ready for implementation plan
**Scope:** Web interface + daemon. The TUI, CLI, and MCP are unchanged.

## Problem

The web agent-detail view shows a **read-only** terminal (1s `tmux capture-pane`
snapshots over SSE) plus a **separate "Send" text field** for input. The user dislikes the
separate field and wants to type **directly into the terminal** and interact with claude
exactly as the TUI does (which runs a real `tmux attach`).

## Goal

Make the focused agent terminal a **genuine interactive `tmux attach`** in the browser:
keystrokes (incl. ctrl-C, arrows, escape sequences), claude's own TUI rendering, and no
input lag — driven by a WebSocket-backed PTY on the daemon. Remove the separate send box.

## Approach

A PTY-backed `tmux attach` bridged over a WebSocket — the canonical web-terminal pattern
(ttyd/wetty). The two lighter alternatives were rejected during brainstorming: forwarding
keystrokes while keeping the 1s snapshot leaves output lagging up to a second and makes
arrow/ctrl/escape translation fiddly and lossy; neither faithfully renders claude's TUI.

When more than one client is attached to the same agent (e.g. the web tab and the TUI),
the **most recently active client drives the size** — the session's tmux `window-size`
option is set to `latest`.

## Design

### 1. Backend — WebSocket attach endpoint

New route `GET /sessions/{id}/attach` (`internal/daemon/attach.go`, handler
`handleAttach`):

1. `store.Get(id)`; if not found, write **404 before any WebSocket upgrade** (so the error
   is a normal HTTP response — and unit-testable).
2. Set the session option `tmux set-option -t <tmuxSession> window-size latest`
   (idempotent; covers already-running agents).
3. Upgrade via `websocket.Accept` (coder/websocket; default same-origin Origin/Host check).
   Raise the read limit (`conn.SetReadLimit(1 << 20)`) so large pastes aren't truncated.
4. Start a PTY running `exec.CommandContext(ctx, "tmux", "attach-session", "-t", <tmuxSession>)`
   via `pty.Start` (github.com/creack/pty).
5. Two pumps until either side errors or the request context ends:
   - **PTY → client:** read PTY bytes, `conn.Write(ctx, websocket.MessageBinary, chunk)`.
   - **client → PTY:** `conn.Read(ctx)`; **binary** messages are keystrokes → write to the
     PTY; **text** messages are resize control frames `{"cols":N,"rows":M}` → `pty.Setsize`.
   Splitting keystrokes (binary) from resize (text) avoids any custom byte framing.
6. On close (client navigates away, or the agent's tmux session dies): cancel the context →
   `exec.CommandContext` kills the `tmux attach` process (detaches **this** client only; the
   agent session and other clients are unaffected), close the PTY and the WebSocket.

A pure helper `parseResize(data []byte) (cols, rows uint16, ok bool)` parses the resize JSON
and is unit-tested.

Route registered in `internal/daemon/lifecycle_routes.go` next to the other
`/sessions/{id}/…` routes.

**New Go dependencies:** `github.com/creack/pty`, `github.com/coder/websocket`.

### 2. Frontend — interactive terminal replaces snapshot + send box

New `web/src/components/AttachTerminal.tsx` (props `{ id: string }`):
- Opens `new WebSocket(attachURL(window.location, id))` with `binaryType = "arraybuffer"`.
- xterm.js + `FitAddon` (both already dependencies).
- `term.onData(d => ws.send(new TextEncoder().encode(d)))` — keystrokes as **binary**.
- `ws.onmessage` (binary) → `term.write(new Uint8Array(e.data))`.
- On open and on `ResizeObserver`/fit: `ws.send(resizeMessage(cols, rows))` — resize as
  **text** JSON. An initial resize is sent immediately after `open`.
- `term.focus()` on click so the user types straight into claude.
- On `ws.close`/`error`, show a small "disconnected — agent may have ended" indicator. No
  auto-reconnect (YAGNI; re-selecting the agent reopens).
- Cleanup on unmount / `id` change: close the socket, dispose the terminal, disconnect the
  observer.

Two pure helpers in new `web/src/lib/attach.ts` (unit-tested):
- `attachURL(loc: { protocol: string; host: string }, id: string): string` — builds
  `ws://`/`wss://` from `http:`/`https:` + `/sessions/<encoded id>/attach`.
- `resizeMessage(cols: number, rows: number): string` — `JSON.stringify({ cols, rows })`.

`web/src/components/AgentTab.tsx`: render `<AttachTerminal id={session.id} />` in place of
the old `<Terminal>`, and **delete the entire send-box section** (and its `msg`/`sending`
state and `sendInput` import). Header, details/timeline, and terminate controls stay.

### 3. Remove the superseded snapshot path

The 1s-snapshot path is now dead and is removed:
- Delete `web/src/components/Terminal.tsx`.
- Remove `subscribeOutput` from `web/src/lib/api.ts`.
- Delete `internal/daemon/output_stream.go` + `internal/daemon/output_stream_test.go` and
  unregister the `GET /sessions/{id}/output/stream` route.
- Remove `OutputANSI` from the `Lifecycle` interface (`internal/daemon/api.go`), the adapter
  (`internal/daemon/lifecycle_adapter.go`), the implementation
  (`internal/lifecycle/lifecycle.go`), its test, and the `fakeLife` stub
  (`internal/daemon/lifecycle_routes_test.go`).

**Kept:** the plain `GET /sessions/{id}/output` + `Lifecycle.Output` + `getOutput` — the
cockpit/overview **mini-tiles keep their cheap read-only poll** (a real PTY attach per tile
would be far too heavy). The `POST /sessions/{id}/input` endpoint stays — the CLI `send`,
MCP `send_to_agent`, and TUI `s` still use it; only the **web** send box is removed.

## Testing

- **Backend unit:** `parseResize` (valid frame, missing fields, malformed JSON, zero/oversize
  values); `handleAttach` returns **404 for an unknown session** before upgrade (httptest,
  plain GET). The PTY ↔ WebSocket byte pump and live `tmux attach` are **manually
  smoke-tested** — consistent with how the repo already treats live tmux/claude paths.
- **Frontend unit (Vitest):** `attachURL` (http→ws, https→wss, id encoding); `resizeMessage`
  (shape). `AttachTerminal` is build-verified (`tsc --noEmit` + `npm run build`); no
  component-render tests, per repo convention.
- Full suites green: `go build ./... && go test ./internal/daemon/ ./internal/lifecycle/`;
  `cd web && npx tsc --noEmit && npm run build && npx vitest run`.

## Security

Localhost-only, single-user — the same threat model as the existing daemon, which already
serves the UI and exposes `POST /input` (arbitrary keystrokes) and `/spawn`. The attach
socket is same-origin; `coder/websocket` enforces the Origin/Host match by default. No new
exposure beyond what `agentctl attach` / the existing input endpoint already allow.

## Risks

- **Adding Go modules requires a module download** (`go get github.com/creack/pty
  github.com/coder/websocket` → `go mod tidy`). If the build sandbox blocks network, this
  step must run where the proxy is reachable (e.g. the user runs it, or `GOPROXY`/module
  cache is available). Flagged so implementation doesn't stall silently.
- Multiple simultaneous attaches share one tmux session; `window-size latest` keeps sizing
  sane but rapid switching can briefly reflow. Acceptable for a single-user tool.

## Out of scope

- Interactive (writable) cockpit mini-tiles — they remain read-only polls.
- Auth / multi-user / non-localhost exposure.
- Auto-reconnect / scrollback persistence across reconnects.
- Changes to the TUI, CLI, or MCP input paths.
