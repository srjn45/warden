---
title: The TUI cockpit
description: The tmux-composited cockpit — agents list, embedded master Claude, and a live agent detail pane.
---

```sh
warden tui   # open the cockpit
warden       # bare invocation — same thing
```

`warden tui` (or bare `warden`) opens a **tmux-composited cockpit** — a dedicated tmux session with three panes: an agents list (top-left), an embedded interactive **master Claude** session wired to the `warden` MCP server (bottom-left), and a full-height live detail pane (right) that opens the selected agent's interactive `claude` session. Browse the list freely with `↑`/`↓` without disturbing the detail pane; press `Enter` to open an agent in it.

```
┌─ Agents (3) ──────┐┌─ agent-4f98 ──────────────┐
│ ▸ agent-4f98  ●   ││                           │
│   agent-c860  ⠿   ││  (live agent session)     │
│   agent-d01c  ✔   ││                           │
├─ Master Claude ───┤│ ...                       │
│ > triage all my   ││                           │
│   agents and tell ││                           │
│   me which are    ││                           │
│   stuck_          ││                           │
└───────────────────┘└───────────────────────────┘
```

![The warden cockpit: an agents list grouped by directory with live status and context size, an embedded master Claude pane below it, and the selected agent's live session in the full-height detail pane on the right.](/warden/media/cockpit.png)

## Cockpit features

| Feature | Description |
|---|---|
| **Live list** | Polls the daemon ~1×/sec; browse with `↑`/`↓` without disturbing the detail pane. |
| **Pipeline tree** | Pipelines shown as a collapsible `▸ Pipelines` section; expand/collapse, open running jobs, retry failed jobs. |
| **Directory groups** | `o` opens a directory as a group (becomes the spawn target for `n`), with `/fs/dirs` tab-completion. |
| **In-cockpit actions** | `n` new agent, `s` send, `a` attach (full-screen), `d` digest overlay, `i` approvals, `c` context/message inspector, `x` terminate/cancel, `D` delete pipeline record, `?` help. |
| **Master-pane shell toggle** | `Ctrl-b t` swaps the bottom-left master Claude pane ↔ a shell (both stay alive, self-heals on shell exit). |
| **Pane focus** | Move focus with `Alt+←/→/↑/↓` (no tmux prefix). |
| **Native scrolling** | Per-agent tmux sessions enable `mouse on` + raised `history-limit` for wheel/copy-mode scrolling of long output. |

## Keys (cockpit)

| Key | Action |
|---|---|
| `↑` / `↓` or `j` / `k` | Move selection (detail pane is unaffected) |
| `←` / `→` or `h` / `l` | Collapse / expand the pipeline under the cursor |
| `Enter` | Open the selected agent (or running pipeline job) in the right detail pane |
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

## Getting back from an attach

Attaching moves your single tmux client onto the agent's session (tmux can't nest an attach), so use **`Ctrl-b Enter`** to jump back to the dashboard — not `Ctrl-b d`. `Ctrl-b d` still works but it *detaches* the cockpit to the background rather than returning to it; the cockpit survives (it's reaped on your next `warden tui`), so an accidental detach no longer destroys your dashboard. Only `q` tears it down.

## Requirements

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes. There is no single-pane fallback: if tmux isn't installed, or you run `warden tui` from **inside an existing tmux session** (which would nest sessions), the cockpit can't build its panes and exits with an error. Run it from a plain terminal. The list pane polls the daemon about once a second, so the daemon must be running before you open the TUI.
