---
title: The TUI cockpit
description: The tmux-composited cockpit — a control-pane navigator tree, a terminal pane, and a live agent pane.
---

```sh
warden tui   # open the cockpit
warden       # bare invocation — same thing
```

`warden tui` (or bare `warden`) opens a **tmux-composited cockpit** — a dedicated tmux session with three panes: a **control pane** (top-left) — a navigator tree of four fixed collapsible sections, **Approvals · Pipelines · Agents · Terminals** — a **terminal pane** (bottom-left) that shows a live terminal session, and a full-height **agent pane** (right) that opens the selected agent's interactive session (the `claude` process by default). A **default terminal** opens in the launch directory at startup, listed under **Terminals** and shown in the terminal pane. Browse the tree freely with `↑`/`↓` without disturbing the agent pane; press `Enter` to open an agent in it.

```
┌─ Control ─────────┐┌─ agent-4f98 ──────────────┐
│ ▾ Approvals (1)   ││                           │
│     agent-c860 ⏳ ││                           │
│ ▾ Pipelines       ││  (live agent session)     │
│     ci-refactor   ││                           │
│ ▾ Agents (3)      ││                           │
│   ▸ agent-4f98 ●  ││ ...                       │
│     agent-c860 ⠿  ││                           │
│     agent-d01c ✔  ││                           │
│ ▾ Terminals (1)   ││                           │
│     term-home     ││                           │
├─ Terminal ────────┤│                           │
│ $ warden ls       ││                           │
│ $ _               ││                           │
└───────────────────┘└───────────────────────────┘
```

![The warden cockpit: a control-pane navigator tree with Approvals, Pipelines, Agents, and Terminals sections on the left, a live terminal session in the terminal pane below it, and the selected agent's live session in the full-height agent pane on the right.](/warden/media/cockpit.png)

## Cockpit features

| Feature | Description |
|---|---|
| **Live control tree** | Polls the daemon ~1×/sec; browse with `↑`/`↓` without disturbing the agent pane. The **Agents** section shows each row's compact **backend** token (claude/aider/…) and the inspector (`i`) adds a `backend` line; an agent with no recorded backend reads as **claude**. |
| **Four fixed sections** | The control pane is a navigator tree of four fixed collapsible sections in order — **Approvals · Pipelines · Agents · Terminals**. Approvals is a persistent section (not an overlay). |
| **Pipelines section** | Pipelines are the collapsible **Pipelines** section of the control tree; expand/collapse, open running jobs, retry failed jobs. |
| **Terminals section** | First-class terminal sessions (`kind=terminal`) live under the **Terminals** section; a default terminal opens at startup. |
| **Agent sub-trees** | Agents spawned by another agent nest under their parent as a collapsible sub-tree (`▸ / ▾`, indented per depth); `h`/`l` toggles. See [Agent sub-trees](#agent-sub-trees) below. |
| **Directory groups** | `o` opens a directory as a group (becomes the spawn target for `n`), with `/fs/dirs` tab-completion. |
| **In-cockpit actions** | `n` new agent, `t` new/focus terminal, `s` send, `a` attach (full-screen), `d` digest overlay, `i` approvals, `c` context/message inspector, `x` terminate/cancel, `D` delete pipeline record, `?` help. |
| **Terminal pane** | Bottom-left pane shows a live terminal session (`kind=terminal`) — a `$SHELL` in a managed worktree for direct CLI access to `warden` commands and other terminal work. A default terminal opens in the launch directory at startup. |
| **Pane focus** | Move focus with `Alt+←/→/↑/↓` (no tmux prefix). **Global Alt rotation** works from any pane, even while typing: **M-t** cycles the terminal pane over all live terminals, **M-a** cycles the agent pane over all live agents, **M-p** cycles the agent pane over pipeline agents (pipeline order). Add **Shift** (`M-T`/`M-A`/`M-P`) to rotate in reverse; each rotation grabs focus on the pane it drives. |
| **Opened marker (◆)** | The agent (Agents section or a Pipelines job row) currently shown in the agent pane, and the terminal currently shown in the terminal pane, are marked with a **◆** — and their name carries a bold magenta badge — in the control tree. It tracks both `Enter`-open and the `M-t`/`M-a`/`M-p` rotation, so you can see what's docked even after the cursor moves away. |
| **Native scrolling** | Per-agent tmux sessions enable `mouse on` + raised `history-limit` for wheel/copy-mode scrolling of long output. |

## Keys (cockpit)

| Key | Action |
|---|---|
| `↑` / `↓` or `j` / `k` | Move selection (agent pane is unaffected) |
| `←` / `→` or `h` / `l` | Collapse / expand the control-pane section, pipeline, or agent sub-tree under the cursor |
| `Enter` | Open the selected agent (or running pipeline job) in the right agent pane — a finished agent or tombstone shows its stored detail instead of attaching; `Enter` on a terminal shows it in the terminal pane |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `t` | Open a terminal in the opened agent's directory (`~` if none open) — an inline choice to `(c)reate` a fresh terminal there or `(f)ocus` an existing one in that dir |
| `M-t` / `M-a` / `M-p` | Global rotation (works from any pane, even while typing): **M-t** cycles the terminal pane over live terminals · **M-a** cycles the agent pane over live agents · **M-p** cycles the agent pane over pipeline agents. Each grabs focus on the pane it drives |
| `M-T` / `M-A` / `M-P` | The same three rotations in **reverse** (Alt+Shift) |
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

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes, and there is no single-pane fallback if tmux isn't installed. From a plain terminal it builds its own tmux session and attaches. From **inside an existing tmux session** (where a plain attach would nest), warden detects `$TMUX` and lays the cockpit out as a **native tmux window** in your *current* session instead — a leaner two-pane layout (control + agent, no terminal pane) that uses your own tmux keybindings, copy-mode, and resizing; `q` closes only the cockpit window. In native-window mode the terminal features (default terminal, `t`, `Enter` on a terminal, `M-t`) degrade to a status hint. Force the native window with `warden tui --tmux-native`, or force the classic three-pane own-session cockpit with `env -u TMUX warden tui`. The control pane polls the daemon about once a second, so the daemon must be running before you open the TUI.
