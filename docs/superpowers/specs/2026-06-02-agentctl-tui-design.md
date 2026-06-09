# agentctl Terminal UI (TUI) — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project A** of the terminal-first / Claude-integrated direction (B context ✅ → **A TUI** → C skill+MCP).

---

## 1. Goal

A live, interactive terminal cockpit for agents (k9s/lazygit feel): see all agents and their status live, watch one agent's output + history, and create / send-to / attach / terminate agents — all from the terminal. Launched via `agentctl tui` and bare `agentctl`.

## 2. Key decisions

| Decision | Choice |
|---|---|
| Framework | **Bubble Tea + Lipgloss** (+ `bubbles` widgets: viewport, textarea, textinput). |
| Layout | **Two-pane**: header bar, left agent list, right always-visible detail (output + history), footer keys. |
| Interactivity | **Full management**: navigate, create (n), send (s), attach (a), terminate (x), live-tail. |
| Launch | `agentctl tui` **and** bare `agentctl` (subcommands still work). |
| Data | Talks to the daemon over HTTP via `internal/client` only (never Mongo). |
| Live updates | **Poll** `GET /sessions` (~1s) for the list + `GET /sessions/{id}/output` (~1s) for the selected agent. (SSE exists; polling keeps the Go side simple/reconnect-free, swappable later.) |

## 3. Architecture

New package `internal/tui`, a Bubble Tea program driven by a small `api` interface that `*client.Client` already satisfies:
```go
type api interface {
	List(ctx context.Context) ([]*store.Session, error)
	Output(ctx context.Context, id string, lines int) (string, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Cleanup(ctx context.Context, id string, force, hard bool) error
	Input(ctx context.Context, id, text string) error
}
```
This makes the model unit-testable with a fake (no daemon/terminal).

### Data flow
- On start: a `listCmd` (one `List`) for first paint + a `tea.Tick` that re-issues `listCmd` every ~1s → `sessionsMsg([]*store.Session)`.
- A `tea.Tick` every ~1s issues `outputCmd(selectedID)` → `outputMsg{id, text}` (ignored if the selection changed meanwhile). Output feeds a `viewport`.
- `List` returning `client.ErrDaemonDown` → `errMsg` → the header shows "reconnecting…" / a daemon-down banner; ticks keep retrying.

## 4. Model, components, files

`Model` fields: `api`, `sessions []*store.Session`, `cursor int` (selected index), `selectedID string`, `output string` + `viewport.Model`, `mode` (normal|newAgent|sendMsg|confirmKill|help), `textarea` (new-agent prompt), `textinput` (send message), `killForce bool`, `status string` (footer message), `connected bool`, `w,h int`.

Files (each one responsibility):
- `internal/tui/tui.go` — `Run(client api) error`: builds the program (`tea.NewProgram(..., tea.WithAltScreen())`) and runs it.
- `internal/tui/model.go` — `Model`, `Init`, `Update` (the pure reducer), `View`.
- `internal/tui/keys.go` — `key.Binding`s + help text.
- `internal/tui/cmds.go` — `listCmd`, `outputCmd`, `spawnCmd`, `cleanupCmd`, `inputCmd`, `tickList`, `tickOutput`, `attachCmd` (wraps `tea.ExecProcess`). Each returns a `tea.Cmd` producing a typed msg.
- `internal/tui/list.go` — render the left table (ID · TYPE · badge · AGE · SUBJECT) with the cursor highlight; `age()`/column helpers.
- `internal/tui/detail.go` — render the right pane (id+badge, dir, subject, the output viewport, recent history) + the modal overlays (new-agent textarea, send textinput, kill confirm, help).
- `internal/tui/styles.go` — Lipgloss styles + `badge(status) (label, style)` mirroring the web `status.ts` mapping (busy=green, attention=amber, idle=grey, error=red).
- `internal/cli/tui.go` — `newTUICmd()` (`agentctl tui`); register it, and set the cobra **root `RunE`** to launch the TUI when invoked with no subcommand (`Args: cobra.NoArgs`).

### Layout (Lipgloss, from `tea.WindowSizeMsg`)
Header (1 line: title + `live ●`/`reconnecting…`). Body split ~40/60: left list / right detail (the output viewport sized to the remaining height). Footer (1 line: `n new · s send · a attach · x kill · ↵/tab focus · q quit`). Modes render an overlay (textarea/textinput/confirm) in place of the footer or centered.

## 5. Modes & keybindings

- **normal:** `↑/↓` or `k/j` move the cursor (updates `selectedID`); `tab` toggles focus to the output viewport (`PgUp/PgDn`/`u/d` scroll); `n` → newAgent; `s` → sendMsg; `a` → attach; `x` → confirmKill; `?` → help; `q`/`ctrl+c` → quit.
- **newAgent:** a `textarea`; `ctrl+s` (or `ctrl+enter`) → `spawnCmd(SpawnParams{Prompt: value})`; `esc` cancels. On success, select the new agent.
- **sendMsg:** a `textinput` prefilled empty; `enter` → `inputCmd(selectedID, value)`; `esc` cancels.
- **attach:** `attachCmd(selectedID)` = `tea.ExecProcess(exec.Command("tmux","attach","-t",id), func(err) tea.Msg{...})` — releases the terminal, runs tmux attach, resumes the TUI and shows any error in the status line.
- **confirmKill:** prompt `Terminate <id>? y/N`; `y` → `cleanupCmd(id,false,false)`. If that returns a 409 (guard), switch to a `killForce` prompt "uncommitted/unpushed — press X to force, esc to cancel"; `X` → `cleanupCmd(id,true,false)`. Success clears selection to the next agent.
- **help:** an overlay listing keys; any key closes it.

## 6. Error handling
- Client errors → `status` footer line (e.g. "send failed: …"); cleared on the next successful action or keystroke.
- `ErrDaemonDown` from the list tick → `connected=false`, header banner, keep ticking.
- Cleanup 409 → the force sub-prompt (see confirmKill).
- Attach error (no tmux / session gone) → status line.
- Empty list → a centered "No agents — press n to create one" hint.
- `outputMsg` whose `id` ≠ current `selectedID` is dropped (stale).

## 7. Testing
- **Update (pure):** table tests feeding msgs to `Update` and asserting model state — cursor moves and clamps at bounds; `sessionsMsg` replaces the list and keeps/repins selection by id; `n`/`s`/`x` switch mode and `esc` returns to normal; `spawnDoneMsg` selects the new id; `cleanupErrMsg{status:409}` → `killForce` prompt; `outputMsg` for the selected id fills output, stale id is ignored; `errMsg(ErrDaemonDown)` sets `connected=false`.
- **Pure helpers:** `badge(status)` mapping table; `age()`; list row / column formatting.
- **cmds:** `spawnCmd`/`cleanupCmd`/`inputCmd`/`listCmd`/`outputCmd` invoke the `api` fake with the right args and return the right msg type (incl. error → `*Msg` carrying the error).
- **View smoke:** `View()` on a model with a few sessions at an 80×24 size returns non-empty output without panic.
- Manual: run `agentctl tui` against a live daemon to verify rendering, scrolling, attach suspend/resume (sandbox can't drive a PTY).

## 8. Out of scope (this sub-project)
- SSE in the Go client (polling for now).
- Mouse support, themes/config, resizing-edge-case polish beyond basic `WindowSizeMsg` handling.
- The Claude skill/MCP (sub-project C).
