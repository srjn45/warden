---
title: The TUI cockpit
description: The tmux-composited cockpit — agents list, terminal shell, and a live agent detail pane.
---

```sh
warden tui   # open the cockpit
warden       # bare invocation — same thing
```

`warden tui` (or bare `warden`) opens a **tmux-composited cockpit** — a dedicated tmux session with three panes: an agents list (top-left), a terminal shell for CLI access (bottom-left), and a full-height live detail pane (right) that opens the selected agent's interactive session (the `claude` process by default). Browse the list freely with `↑`/`↓` without disturbing the detail pane; press `Enter` to open an agent in it.

```
┌─ Agents (3) ──────┐┌─ agent-4f98 ──────────────┐
│ ▸ agent-4f98  ●   ││                           │
│   agent-c860  ⠿   ││  (live agent session)     │
│   agent-d01c  ✔   ││                           │
├─ Shell ───────────┤│ ...                       │
│ $ warden ls       ││                           │
│ agent-4f98  ●     ││                           │
│ agent-c860  ⠿     ││                           │
│ $ _               ││                           │
└───────────────────┘└───────────────────────────┘
```

![The warden cockpit: an agents list grouped by directory with live status and context size, a terminal shell below it, and the selected agent's live session in the full-height detail pane on the right.](/warden/media/cockpit.png)

## Cockpit features

| Feature | Description |
|---|---|
| **Live list** | Polls the daemon ~1×/sec; browse with `↑`/`↓` without disturbing the detail pane. Each row shows a compact **backend** token (claude/aider/…) and the inspector (`i`) adds a `backend` line; an agent with no recorded backend reads as **claude**. |
| **Pipeline tree** | Pipelines shown as a collapsible `▸ Pipelines` section; expand/collapse, open running jobs, retry failed jobs. |
| **Agent sub-trees** | Agents spawned by another agent nest under their parent as a collapsible sub-tree (`▸ / ▾`, indented per depth); `h`/`l` toggles. See [Agent sub-trees](#agent-sub-trees) below. |
| **Directory groups** | `o` opens a directory as a group (becomes the spawn target for `n`), with `/fs/dirs` tab-completion. |
| **In-cockpit actions** | `n` new agent, `s` send, `a` attach (full-screen), `d` digest overlay, `i` approvals, `c` context/message inspector, `x` terminate/cancel, `D` delete pipeline record, `?` help. |
| **Terminal shell pane** | Bottom-left pane runs `$SHELL` for direct CLI access to `warden` commands and other terminal work. |
| **Pane focus** | Move focus with `Alt+←/→/↑/↓` (no tmux prefix). |
| **Native scrolling** | Per-agent tmux sessions enable `mouse on` + raised `history-limit` for wheel/copy-mode scrolling of long output. |

## Keys (cockpit)

| Key | Action |
|---|---|
| `↑` / `↓` or `j` / `k` | Move selection (detail pane is unaffected) |
| `←` / `→` or `h` / `l` | Collapse / expand the pipeline or agent sub-tree under the cursor |
| `Enter` | Open the selected agent (or running pipeline job) in the right detail pane — a finished agent or tombstone shows its stored detail instead of attaching |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `o` | Open a directory as a group (becomes the spawn target for `n`) |
| `s` | Send a message to the selected agent — `enter` to send, `esc` to cancel |
| `a` | Attach — full-screen the agent's (or running job's) tmux session; press **`Ctrl-b Enter`** to return to the dashboard |
| `d` | Completion digest for the selected agent — scrollable overlay; `d`/`esc` to close |
| `i` | Answer pending approvals (also `enter` on the **⏳ Approvals** row) — `1`-`9` to answer, `tab` for next |
| `c` | Shared-context + message-traffic inspector |
| `r` | Retry a failed / needs-attention pipeline job |
| `x` | Context-sensitive: terminate the selected agent / cancel a pipeline / close an opened dir (confirm with `y`) |
| `D` | Delete a stopped pipeline's record (confirm with `y`) |
| `?` | Toggle help overlay |
| `q` | Quit and tear down the cockpit |

## Agent sub-trees

When an agent spawns another agent (via the `spawn_agent` MCP tool), warden records
which agent created it. The cockpit uses that parentage to **nest spawned agents
under the agent that created them**, so an orchestrator and the workers it fanned
out read as one tree instead of a flat, indistinguishable list:

```
┌─ Agents (4) ──────────────────┐
│ ▾ agent-4f98  busy            │   ← orchestrator (root)
│     agent-c860  busy          │   ← spawned by agent-4f98
│   ▾ agent-d01c  busy          │   ← spawned by agent-4f98, has its own child
│       agent-9b22  idle        │   ← spawned by agent-d01c
└───────────────────────────────┘
```

- **Collapsible, arbitrary depth.** Any agent with children shows a `▸ / ▾`
  header — the same affordance pipelines use. Press `h` / `←` to collapse its
  sub-tree, `l` / `→` to expand. Nesting follows the real spawn depth (A → B → C …).
- **Zero change to the flat case.** An agent with no parent and no children looks
  and behaves exactly as before.
- **Tombstones — parents never orphan their children.** If you delete a parent
  that still has live descendants, it does **not** vanish. It stays as a muted
  *terminated tombstone* header showing `terminated · N running`, with no
  terminal/attach pane — exactly like a completed pipeline job renders. The
  daemon reaps the tombstone automatically once the whole sub-tree has gone
  terminal. A parent that simply finishes on its own while children are still
  running renders the same way.
- **`Enter` on a finished agent or tombstone** opens its **stored detail** in the
  right pane (status, location, output, digest) instead of attaching to a dead
  tmux session. A live agent still attaches as usual.

The parentage is also available over the API (`parent_id` on each session), so
other surfaces can mirror the tree.

## Getting back from an attach

Attaching moves your single tmux client onto the agent's session (tmux can't nest an attach), so use **`Ctrl-b Enter`** to jump back to the dashboard — not `Ctrl-b d`. `Ctrl-b d` still works but it *detaches* the cockpit to the background rather than returning to it; the cockpit survives (it's reaped on your next `warden tui`), so an accidental detach no longer destroys your dashboard. Only `q` tears it down.

## Requirements

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes. There is no single-pane fallback: if tmux isn't installed, or you run `warden tui` from **inside an existing tmux session** (which would nest sessions), the cockpit can't build its panes and exits with an error. Run it from a plain terminal. The list pane polls the daemon about once a second, so the daemon must be running before you open the TUI.
