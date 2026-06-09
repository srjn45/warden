# TUI master-Claude pane — tmux-composited cockpit

**Date:** 2026-06-03
**Status:** Approved (design)
**Scope:** Re-architect the TUI from a single bubbletea program into a tmux-composited multi-pane cockpit, and add an embedded "master Claude" pane (bottom-left) that can manage/monitor the whole agent fleet. The broader "full-fledged TUI IDE" is the north-star roadmap; this spec is the first coherent step that proves the architecture.

## Motivation

Today the cockpit is a *viewer*: `agentctl tui` shows a list of agents and the output of the selected one. The user wants a *control surface* — split the left pane vertically so the top half is the agents list and the bottom half is a live Claude session that can manage and monitor the other agents (or do anything Claude Code can do). This is the seed of turning agentctl into a TUI-based IDE for operating a fleet of agents.

Target layout:

```
┌─ Agents (3) ──────┐┌─ agent-4f98 ──────────────┐
│ ▸ agent-4f98  ●   ││ dir: ~/...                │
│   agent-c860  ⠿   ││ subject: refactor poller  │
│   agent-d01c  ✔   ││                           │
├─ Master Claude ───┤│ output ─────────────────  │
│ > triage all my   ││ ...                       │
│   agents and tell ││                           │
│   me which are    ││                           │
│   stuck_          ││                           │
└───────────────────┘└───────────────────────────┘
```

## Current state (what we're changing)

- `cli/tui.go` → `tui.Run(api)` starts **one** bubbletea program (`tea.WithAltScreen()`).
- `internal/tui/model.go` — a single `Model` holding the list, the detail viewport (`vp`), modal inputs (`ta`, `ti`), cursor, selection, and connection state. `Update` is a pure reducer.
- `internal/tui/view.go` — `View()` draws header + body (`titleBox(list)` joined horizontally with `titleBox(detail)`) + footer; modes for new-agent / send / confirm-kill / help.
- Agents run as **tmux sessions**; `attach` drops into one. A daemon is the **single writer** to the file store and the source of truth for the agent list + output. An `agentctl mcp` stdio server lets an orchestrator Claude manage agents programmatically.

The current layout puts list (left ~40%) and detail (right) in **one** app. The new layout (master Claude bottom-left, detail full-height right) makes list and detail separate rectangles — so they can no longer live in one process.

## Decisions (settled during brainstorming)

1. **Master Claude = embedded real `claude` (PTY)** — full Claude Code power in-pane, not a custom chat or a tail/attach mirror.
2. **tmux is the compositor** — `agentctl tui` stops drawing the whole screen. It builds a tmux session with real split panes, each hosting its own process. tmux natively hosts every PTY (this is what tmux is for), reusing the fact that agents + attach already run on tmux.
3. **Scope = the master pane + the tmux re-architecture only.** IDE primitives (diffs, file tree, command palette, bulk ops) are deferred to later specs.
4. **Selection sync = local session-state file**, not a daemon field. List pane writes selection; detail pane reads it. No daemon coupling; naturally scoped per cockpit.
5. **Master lifecycle = ephemeral.** Dies with the TUI; fresh each launch. Persistence is a documented future enhancement (see below).
6. **Per-pid cockpit** — two shells launching `agentctl tui` get two independent cockpits, each with its own ephemeral master. No master-identity problem to solve now.
7. **Keep the legacy single-pane app reachable as `--classic`** — used as the fallback when tmux is unavailable, so no environment is stranded.
8. **Poll `selection.json` on the existing tick** — do not add an fsnotify dependency (YAGNI; can add later).

## Architecture

```
agentctl tui  (launcher — builds & attaches tmux session "agentctl-tui-<pid>")
├─ top-left    pane → agentctl tui --pane=list   --state-dir=<dir>   (bubbletea)
├─ bottom-left pane → claude  (real PTY, wired to agentctl MCP)      ← master
└─ right       pane → agentctl tui --pane=detail --state-dir=<dir>   (bubbletea)
```

- `--pane=list` and `--pane=detail` are **internal/hidden** flags routing into new entrypoints in the `tui` package.
- The session name embeds the launcher pid → independent cockpits per launch.
- tmux becomes a hard runtime dependency for the cockpit (agentctl already requires tmux for agents).

## Components

### Launcher (`internal/tui` + `cli/tui.go`)
Responsibilities:
1. **Nesting guard** — if already inside the agentctl cockpit session (detect via `$TMUX` + session name, or the presence of a `--pane` flag), do not recurse.
2. **State dir** — create `$XDG_RUNTIME_DIR/agentctl/tui-<pid>/` to hold `selection.json`. Best-effort cleanup of stale dirs from crashed prior runs on launch; cleanup own dir on exit.
3. **Build the tmux session** — split-window commands + layout sizing: left column ~40% width, split vertically ~50/50 into list (top) and master (bottom); detail takes the full-height right column.
4. **Wire the master pane** — launch `claude` in the bottom-left pane, configured to talk to the `agentctl mcp` stdio server. The exact mechanism (generated mcp-config file vs. a `claude` CLI flag) **must be verified against the installed `claude` CLI during planning** before implementation.
5. **Attach** — `tmux attach` to the session; block until detached/killed.

The tmux command construction is a **pure function returning the list of tmux commands/args** (unit-testable); the actual exec is a thin wrapper. This mirrors the existing `boxes.go` pure-helper-plus-thin-shell style.

### ListModel (`internal/tui`)
- The current list + action modals: `n` new agent, `s` send message, `a` attach, `x` terminate, `?` help, `q` quit.
- Polls the daemon (`List`) on the existing tick; renders via the reused `titleBox` / `renderList` helpers.
- **On cursor change, writes `selection.json`.**
- `q` kills the tmux session (which tears down the master + cockpit).
- `a` (attach) → `tmux switch-client` to the selected agent's own tmux session; detaching from that returns to the cockpit.

### DetailModel (`internal/tui`)
- Read-only viewport of the selected agent's output.
- On its existing tick: reads `selection.json`, fetches that agent's output from the daemon (`Output`), renders via the reused `renderDetail` helper.
- Empty/missing selection → existing "—" placeholder state.

### Master pane
- Just `claude` — agentctl does **not** render it; tmux hosts the PTY. Wired to the `agentctl mcp` server so it can act on any agent / the whole fleet by id. Needs no selection state.

## Data flow & selection sync

- **Daemon stays the single source of truth** for the agent list and per-agent output. Both panes poll it independently using the existing `tick` / `List` / `Output` paths and the existing "reconnecting…" connection state.
- **Selection** lives in `selection.json` (`{ "id": "agent-4f98", "ts": <unix> }`) under the per-pid state dir. List writes on change; detail reads it on its existing tick. No new dependency; corrupt/missing file → detail falls back to the empty state.
- **Master** acts through the MCP by id; it observes and changes fleet state via the daemon like any orchestrator Claude.

## Error handling / edge cases

- **tmux missing** → clear error message + fall back to the legacy single-pane app (`--classic`).
- **claude binary missing** → master pane shows an error; list/detail cockpit still functions.
- **daemon down** → each pane independently shows the existing "reconnecting…" state.
- **`selection.json` missing/corrupt** → detail shows the "—" placeholder.
- **Crash/stale state dirs** → cleaned best-effort on next launch.
- **Quit** → `q` kills the tmux session; ephemeral master dies with it; own state dir removed.

## Testing

- `ListModel` and `DetailModel` remain pure bubbletea reducers → unit-tested like the existing `model_test.go`.
- `selection.json` read/write → unit tests for round-trip + corrupt/missing-file handling.
- tmux-command-builder pure function → unit-tested on the produced args; exec stays a thin, un-unit-tested shell.
- Manual integration: launch the cockpit → verify three panes render, selection in the list updates the detail pane, and the master Claude can drive a real agent (spawn / send / terminate) via MCP.

## Future enhancement (documented, not built): persistent master

Ephemeral is the chosen first step because it is the smallest change that proves the tmux-compositor + embedded-PTY architecture — the real risk. Persistence is a clean later layer: promote the master from "a plain pane" to "a special agentctl-managed session," reusing the existing `start` / `restore` (`claude --resume`) / `attach` machinery. Deferring it paints no architectural corner.

What persistence would buy: fleet memory across sessions, survival of long-running orchestration ("watch all agents and ping me when one finishes"), and crash resilience for the commander's context.

Identity model — the open question to resolve before building persistence:

- **Global singleton (recommended for a global fleet):** one fixed session (e.g. `agentctl-master`), independent of dir/shell. Two shells share the **same** master. Restore = "is `agentctl-master` alive? → `tmux attach` + `claude --resume` : create new." Coherent because the fleet itself is global (one daemon; the list shows all agents regardless of launch dir). Caveats: two attached TUIs **mirror** the same pane (standard `tmux attach` semantics — same cursor/view; mildly chaotic if typing in both), and the master needs a fixed home `cwd` (e.g. `~` or a configured `master_workdir`), not the launching shell's dir.
- **Per-directory:** key = hash of `cwd`; same dir shares/restores, different dirs get different masters. Rejected for now because a project-scoped brain commanding a machine-wide fleet is incoherent given the global daemon — but recorded here in case the fleet model ever becomes per-project.

Restore-vs-new, in general: on launch, compute the key, look it up (store record / live tmux session); if a live master with that key exists → attach + `claude --resume`, else create new.

## Out of scope (north-star roadmap, later specs)

Live worktree diff view, multi-select agents for bulk actions, logs/output follow mode, file tree, command palette, configurable layouts.
