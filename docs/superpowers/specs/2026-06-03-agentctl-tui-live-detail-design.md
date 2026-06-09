# TUI live detail pane — interactive agent terminal

**Date:** 2026-06-03
**Status:** Approved (design)
**Builds on:** `2026-06-03-agentctl-tui-master-pane-design.md` (the tmux-composited cockpit).

## Motivation

After using the cockpit, three pieces of feedback:

1. The right (detail) pane is **read-only** — you can watch the selected agent's output but can't type into it. The user wants to interact with the agent's terminal directly from the cockpit, the same way the bottom-left master pane lets you type into a live `claude`. (`attach` is liked and stays, but typing small prompts directly from the TUI is the day-to-day need.)
2. The **history** block at the bottom of the detail pane is always empty and should be removed for now.
3. Switching panes currently requires the mouse; the user wants a **keyboard shortcut**.

## Decisions (settled during brainstorming)

1. **Detail pane becomes a live, interactive terminal of the selected agent** — not an inline composer, not a read-only viewer. You type straight into the agent's session.
2. **Mechanism = live nested attach, not join-pane.** The pane runs `env -u TMUX tmux attach -t <agent-session>`. `join-pane` was rejected because it *moves* the agent's pane out of its own session, breaking the daemon (which reads/sends by session name) and `attach`.
3. **Interaction = Enter-to-open, not auto-follow.** Browsing the list with j/k must NOT respawn the detail terminal (that would kill an active agent session mid-keystroke). Pressing **Enter** on the highlighted agent opens it in the detail pane.
4. **Pane switching = prefix-less Alt+Arrow** bindings.
5. **Keep `a`** (full-screen `switch-client` to the agent) unchanged.
6. **Remove** the history block, and — now superseded — `selection.json` and the detail bubbletea viewer.
7. **Same tmux server** as the agents (accept the nested-tmux caveats below) rather than isolating the cockpit on its own socket. Socket isolation is a possible future enhancement.

## Architecture

The cockpit layout is unchanged (list top-left, master bottom-left, detail right/full-height). What changes is **the detail pane's content and how it's driven**:

- **Before:** detail pane ran `agentctl tui --pane=detail --state-dir=<dir>` (a read-only bubbletea viewer) and followed `selection.json`, which the list pane wrote on every cursor move.
- **After:** detail pane starts as a small **placeholder** process. The **list pane** holds the detail pane's tmux id and, on **Enter**, respawns the detail pane in place to a live attach to the selected agent. The file-based selection handoff and the detail viewer are removed.

```
list pane (top-left, bubbletea)
  ├─ j/k: browse (no side effect on detail)
  ├─ Enter: tmux respawn-pane -k -t <detailPaneId> "env -u TMUX tmux attach -t <agent>"
  ├─ s / x / n / a / ? / q: unchanged
detail pane (right, full height)
  └─ placeholder  →(Enter)→  live `tmux attach -t <agent>`  (interactive agent terminal)
master pane (bottom-left): live claude (unchanged)
```

## Components

### `buildCockpit` (internal/tui/compositor.go)
- The right pane is created running a **placeholder** command instead of `agentctl tui --pane=detail`. Placeholder: a process that keeps the pane alive and prints a hint, e.g. `sh -c 'printf "Select an agent and press Enter to open it here.\n"; exec sleep 2147483647'`.
- After creating panes, set the detail pane to **`remain-on-exit on`** so that when an opened agent's attach exits (agent dies, or the user detaches the inner client) the pane shows `[exited]` rather than collapsing the layout.
- Add the **Alt+Arrow** pane-navigation bindings (prefix-less): `bind-key -n M-Left select-pane -L`, `-R`, `-U`, `-D`. (NOTE: tmux `-n` bindings are server-global; see caveats.)
- Pass the **detail pane id** to the list pane via a new flag `--detail-pane=<paneId>` on the `--pane=list` invocation.

### List pane (internal/tui/list_pane.go)
- Accepts the detail pane id (`detailPane string` field, set from the new flag).
- **Enter** (normal mode): if an agent is selected, run a `respawnDetailCmd(detailPane, agentSession)` tea.Cmd that executes `tmux respawn-pane -k -t <detailPane> "env -u TMUX tmux attach -t <agent>"`. The agent's tmux session name equals the agent id (store `TmuxSession == ID`).
- **j/k** no longer write any selection state (the `selection.json` write is removed).
- `s`, `x`, `n`, `a`, `?`, `q` unchanged. `a` remains `switch-client`.

### Detail pane + selection state — removed
- Delete `internal/tui/detail_pane.go`, its tests, the `--pane=detail` dispatch branch, and `RunDetailPane`.
- Delete `internal/tui/selection.go` and its tests (no longer used).
- **Remove the entire per-pid state-dir subsystem**, since `selection.json` was its only content: drop `cockpitBaseDir`, `cockpitStateDir`, `cleanStaleStateDirs`, `pidAlive`, the `--state-dir` flag, and the `MkdirAll`/`defer RemoveAll`/`cleanStaleStateDirs` calls in `RunCockpit`. Keep `cockpitSession` (the per-pid tmux session name is still used). `RunCockpit`'s signature is unchanged otherwise; it just no longer manages a state dir.

### renderDetail / history (internal/tui/detail.go, view.go)
- Remove the `hist`/`renderHistory` block from `renderDetail`. `renderDetail` is still used by the **classic** single-pane TUI (`--classic`), so it stays — just without history. Recompute `detailChrome` accordingly (drops by the ~7 history lines).
- `renderHistory` becomes dead code → delete it.

### CLI (internal/cli/tui.go)
- `--pane=detail` branch removed (only `list` remains as an internal pane). Add/forward the `--detail-pane` value into `RunListPane`.

## Data flow

- **No more `selection.json`.** The list pane drives the detail pane imperatively via `tmux respawn-pane`. The only cross-pane state is the detail pane id, passed once at launch.
- The agent attach in the detail pane talks to the **same tmux server** as the agents, so it sees the agent's live session directly. The daemon is unaffected — it still owns the agent session; the cockpit is just another (nested) viewer/typer.

## Error handling / edge cases

- **No agent / empty list:** detail pane shows the placeholder until Enter is pressed on a real agent.
- **Opening a dead agent (no such tmux session):** `tmux attach -t <gone>` exits immediately; with `remain-on-exit on` the pane shows `[exited]`. Pressing Enter on a live agent respawns it fine.
- **Agent dies while open:** the inner attach exits → pane shows `[exited]` (layout preserved). Re-open another agent with Enter.
- **Nested attach refused:** `env -u TMUX` removes the guard; verified in the manual checklist.
- **classic mode (`--classic`)** is unaffected by the detail-pane changes except that its detail view no longer shows history.

## Caveats accepted

- **Nested-tmux prefix ambiguity** in the detail pane (`Ctrl-b` goes to the inner agent client). Mitigated by prefix-less Alt+Arrow navigation and the fact that you mostly just type into the agent's claude.
- **Alt+Arrow binds are tmux server-global** (tmux can't scope `-n` binds per session), so they also affect other sessions on the same server. Acceptable; socket isolation is a future option.
- **Resize negotiation:** an agent session attached in a small cockpit pane *and* elsewhere negotiates size to the smallest client. Known nested-attach behavior; acceptable.

## Testing

- **Unit:** `respawnDetailCmd`'s command construction extracted as a pure function and asserted (argv: `tmux respawn-pane -k -t <paneId> <attach-cmd-string>`), mirroring `buildCockpit`'s FakeRunner tests. The list pane's Enter handler is exercised in a reducer test (Enter with a selected agent triggers the respawn command; Enter with no selection is a no-op). `buildCockpit` test updated for the placeholder command, `remain-on-exit`, the Alt+Arrow binds, and the `--detail-pane` flag on the list pane command.
- **Manual integration:** launch cockpit → Enter on an agent opens its live terminal in the right pane → type a prompt, see the agent respond → Alt+Arrow moves between all three panes → `a` still gives full-screen attach → killing an opened agent leaves `[exited]`, layout intact → `q` tears down the cockpit.

## Out of scope (future)

Cockpit on its own tmux socket (to scope bindings and dodge the nested prefix), an inline-composer alternative, auto-follow mode, restoring a (non-empty) history view.
