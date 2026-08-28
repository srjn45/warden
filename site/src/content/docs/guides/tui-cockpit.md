---
title: The TUI cockpit
description: The projects-first tmux-composited cockpit — a Projects frame, a Terminals frame, a live agent pane, and the Open Project panel.
---

```sh
warden tui   # open the cockpit
warden       # bare invocation — same thing
```

`warden tui` (or bare `warden`) opens a **tmux-composited cockpit** — a dedicated tmux session with three panes: a **control pane** (top-left), a **terminal pane** (bottom-left) that shows a live terminal session, and a full-height **agent pane** (right) that opens the selected agent's interactive session (the `claude` process by default). The control pane is **projects-first**: two bordered inner frames — **Projects** (agents grouped under their git project node, with each agent's subagents and pipelines nested beneath it) and **Terminals** (a flat, non-project-scoped list). A **default terminal** opens in the launch directory at startup, listed under **Terminals** and shown in the terminal pane. Browse the tree freely with `↑`/`↓` without disturbing the agent pane; press `Enter` to open an agent in it.

The old always-present **Approvals and Pipelines top-level sections are gone**: a blocked agent surfaces through its **needs-input** status (press `p` to answer), and every pipeline renders **inside the Projects frame** — a delegated pipeline under its owning orchestrator, a human/orchestrator-less one under its project node.

```
┌─ Control ─────────────┐┌─ agent-4f98 ──────────────┐
│ ┌ Projects ─────────┐ ││                           │
│ │ warden (git)      │ ││  (live agent session)     │
│ │   agent-4f98 ●    │ ││                           │
│ │     subagent      │ ││ ...                       │
│ │     ci-refactor   │ ││                           │
│ │       job-1       │ ││                           │
│ │   agent-c860 ⠿    │ ││                           │
│ │ rdq (git)         │ ││                           │
│ │   orch-agent      │ ││                           │
│ └───────────────────┘ ││                           │
│ ┌ Terminals ────────┐ ││                           │
│ │ 1. warden (main)  │ ││                           │
│ └───────────────────┘ ││                           │
├─ Terminal ────────────┤│                           │
│ $ warden ls  _        ││                           │
└───────────────────────┘└───────────────────────────┘
```

Inside **Projects**, agents are grouped under their **Project** node — the agent's worktree's git project, keyed by the canonical remote URL (with a `local:` fallback for a remoteless repo; the **same normalizer as [collaboration groups](/warden/multi-agent/collaboration-groups/)**). Multiple worktrees of one repo collapse to a single project node. Top-level agents list directly under the project; each agent's subagents and pipelines nest beneath it. Cursor-home lands on the first agent under the first project.

![The warden cockpit: a projects-first control pane with a Projects frame grouping agents by git project and a Terminals frame on the left, a live terminal session in the terminal pane below it, and the selected agent's live session in the full-height agent pane on the right.](/warden/media/cockpit.png)

## Cockpit features

| Feature | Description |
|---|---|
| **Live control tree** | Polls the daemon ~1×/sec; browse with `↑`/`↓` without disturbing the agent pane. The **Projects** frame shows each agent row's compact **backend** token (claude/aider/…); the full **agent info** pane (`i`) lists every stored field — backend, model, role, tags, context, location, refs, rate-limit, lifecycle, plumbing, and the last pane excerpt. An agent with no recorded backend reads as **claude**. |
| **Projects frame** | Agents are grouped under their **Project** node — the agent's worktree's git project, keyed by the canonical remote URL (with a `local:` fallback for a remoteless repo — the same normalizer as collaboration groups). Multiple worktrees of one repo collapse to a single project node. The "dir" vocabulary is renamed **Project** throughout the TUI. |
| **Pipelines inside Projects** | There is no top-level Pipelines section. A **delegated** pipeline (carrying an owner link) nests **under its owning orchestrator**; a **human/orchestrator-less** one renders **under its project node**. Each is a header with its job children (`(deps: …)`-annotated); expand/collapse, open running jobs, `r` retries a failed job. |
| **Approvals via needs-input** | No top-level Approvals section — a worker blocked on a prompt surfaces through its agent node's **needs-input** status; press `p` (or `enter` on the ⏳ row) to answer. A worker that has *finished and sits at an idle prompt* reads as **idle/done**, not `needs-input`. |
| **Terminals frame** | First-class terminal sessions (`kind=terminal`) live in a flat, non-project-scoped **Terminals** frame (a `cd` inside a terminal roams across projects); a default terminal opens at startup. |
| **Open Project panel (`o`)** | `o` takes over the whole control pane with a full-pane panel: a persisted **recent-projects** list, **open local** (`l`, dir navigator), and **open via git** (`g`, clone into `~/.warden/workspace/<project>`). Opening any project **auto-spawns its single orchestrator** (one-per-project — an existing one is *focused*, not duplicated); `Esc` returns to Projects. See [Open Project](#open-project) below. |
| **Agent sub-trees** | Agents spawned by another agent nest under their parent as a collapsible sub-tree (`▸ / ▾`, indented per depth) inside the parent's project; `h`/`l` toggles. See [Agent sub-trees](#agent-sub-trees) below. |
| **Agent info + editing** | `i` opens the **agent info** pane — every stored field for the selected agent, plus interactive controls to **toggle auto-approve**, **cycle force-compact** (inherit → on → off), and open the **event log** (`e`). See [Agent info pane](#agent-info-pane) below. |
| **In-cockpit actions** | `n` new agent, `t` new/focus terminal, `o` Open Project, `s` send, `a` attach (full-screen), `d` digest overlay, `i` agent/pipeline detail, `e` event log, `p` approvals, `b` backends, `ctrl+a` autopilot, `c` context/message inspector, `r` retry job, `x` kill/cancel/close, `D` delete pipeline record, `?` help. |
| **Terminal pane** | Bottom-left pane shows a live terminal session (`kind=terminal`) — a `$SHELL` in a managed worktree for direct CLI access to `warden` commands and other terminal work. A default terminal opens in the launch directory at startup. |
| **Pane focus** | Move focus with `Alt+←/→/↑/↓` (no tmux prefix). **Global Alt rotation** works from any pane, even while typing: **M-t** cycles the terminal pane over all live terminals, **M-a** cycles the agent pane over the **Projects frame's** live agents. Add **Shift** (`M-T`/`M-A`) to rotate in reverse; each rotation grabs focus on the pane it drives. (**M-p** is retired — pipelines are reached inside the Projects tree.) On terminals that don't send Alt/Option as Meta — **macOS Terminal.app and iTerm2 by default** — use the config-free `Ctrl-b` prefix fallback: press `Ctrl-b` then `t`/`a` (add Shift for reverse). See [macOS: the Option key](#macos-the-option-key). |
| **Opened marker (◆)** | The agent (Projects frame or a nested pipeline job row) currently shown in the agent pane, and the terminal currently shown in the terminal pane, are marked with a **◆** — and their name carries a bold magenta badge — in the control tree. It tracks both `Enter`-open and the `M-t`/`M-a` rotation, so you can see what's docked even after the cursor moves away. |
| **Native scrolling** | Per-agent tmux sessions enable `mouse on` + raised `history-limit` for wheel/copy-mode scrolling of long output. |

## Keys (cockpit)

| Key | Action |
|---|---|
| `↑` / `↓` or `j` / `k` | Move selection (agent pane is unaffected) |
| `←` / `→` or `h` / `l` | Collapse / expand a frame (Projects·Terminals), a pipeline, or an agent sub-tree under the cursor |
| `Enter` | Open the selected agent (or running pipeline job) in the right agent pane — a finished agent or tombstone shows its stored detail instead of attaching; `Enter` on a terminal shows it in the terminal pane |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `t` | Open a terminal in the opened agent's directory (`~` if none open) — an inline choice to `(c)reate` a fresh terminal there or `(f)ocus` an existing one in that dir |
| `o` | **Open Project** — full-pane panel: recent list · `l` open local (dir navigator) · `g` open via git (clone into `~/.warden/workspace/<project>`). Opening a project auto-spawns (or focuses) its single orchestrator; `Esc` back. See [Open Project](#open-project) |
| `M-t` / `M-a` | Global rotation (works from any pane, even while typing): **M-t** cycles the terminal pane over live terminals · **M-a** cycles the agent pane over the Projects frame's live agents. Each grabs focus on the pane it drives (**M-p** is retired) |
| `M-T` / `M-A` | The same two rotations in **reverse** (Alt+Shift) |
| `Ctrl-b` then `t`/`a` | Config-free rotation fallback (add Shift for reverse) — identical to `M-t`/`M-a`, but via the tmux prefix so it works on terminals that don't send Alt/Option as Meta (**macOS Terminal.app / iTerm2** default). See [macOS: the Option key](#macos-the-option-key) |
| `s` | Send a message to the selected agent — `enter` to send, `esc` to cancel |
| `a` | Attach — full-screen the agent's (or running job's) tmux session; press **`Ctrl-b Enter`** to return to the dashboard |
| `d` | Completion digest for the selected agent — scrollable overlay; `d`/`esc` to close |
| `i` | **Details** for the selected agent or pipeline job — a scrollable pane showing every stored field, plus three interactive controls: `↑`/`↓` walk the control cursor and then scroll the body once past the last control, **`space`** toggles **auto-approve** and cycles **force-compact** (inherit → on → off), and **`enter`** on the **events** row (or **`e`**) opens the event log. `pgup`/`pgdn`/`g`/`G` also scroll · `r` rename · `i`/`esc` back |
| `e` | From agent info: open the selected agent's **event log** in the control pane (newest first); `e`/`esc` returns to agent info |
| `p` | Answer pending approvals (also `enter` on the **⏳** row) — `1`-`9` to answer, `tab` for next |
| `c` | Shared-context + message-traffic inspector |
| `b` | Agent-backend registry page (tier / default / enable · `r` rescan · `m` thinking-mode) |
| `r` | Retry a failed / needs-attention pipeline job |
| `x` | Context-sensitive: kill the selected agent / cancel a pipeline / close a terminal / close an opened project (confirm with `y`) |
| `D` | Delete a stopped pipeline's record (confirm with `y`) |
| `ctrl+a` | Toggle autopilot on/off (run `warden autopilot init` first if not configured) |
| `?` | Toggle help overlay |
| `q` | Quit and tear down the cockpit |

## Agent info pane

Pressing **`i`** on an agent opens the **agent info** pane — a scrollable, read-most
view of everything warden knows about that agent. It replaces the old terse detail
overlay and now surfaces every stored field, grouped into sections:

- **controls** — the three interactive rows (see below).
- **summary** — name, subject, type, backend, **model**, **role**, **tags**, context
  fill (with when it was last checked), and creation time.
- **location** — repo/workdir, worktree (and whether warden created or adopted it),
  and branch.
- **refs** — ticket, PR, owning pipeline/job, and **parent** agent.
- **rate-limit** — shown only when the agent has hit a limit: when it started, the
  scheduled resume time, and the retry count.
- **lifecycle** — auto-restart count, last restart, last auto-`/compact`.
- **plumbing** — pid, tmux session, exit code, backend session id, initial prompt.
- **pane** — the last captured pane excerpt.

The top **controls** block is interactive — move the cursor with `↑`/`↓` and act
with `space` / `enter`. The arrows are scroll-aware: they first walk the cursor
through the three control rows, then hand off to line-scrolling the field dump
once you press past the last control, so the whole pane is reachable with the
arrows alone (`↑` scrolls back up and re-enters the controls at the top):

| Control | Action |
|---|---|
| **auto-approve** | `space` toggles this agent's per-agent auto-approve override **on**/**off** (auto-answers yes/no tool prompts with option 1). |
| **force-compact** | `space` cycles the per-agent force-compact override **inherit → on → off** — `inherit` follows the global `token_force_compact`; `on`/`off` override it for this agent. |
| **events** | `enter` (or **`e`** from anywhere in the pane) opens the **event log** — the agent's timestamped history, newest first — in the control pane. `e`/`esc` returns to agent info. |

The navigation nests one level deep: **control tree → (`i`) agent info → (`e`) events**,
and `esc` walks back out one level at a time (events → info → tree). Edits apply
immediately via the daemon (the same paths as `warden approve`-policy and the
`set_force_compact` MCP tool), and the pane re-renders on the next poll so the shown
values and event count stay live. `pgup`/`pgdn`/`g`/`G` scroll the field dump; `r`
renames the agent.

## Open Project

Pressing **`o`** no longer opens the highlighted row — it **takes over the whole control pane** with a full-pane **Open Project** panel offering three ways in:

1. **Recent projects** — a persisted list of previously-opened projects (project key, display name, remote/path, last-opened). `↑`/`↓` to move, `Enter` to open — re-focusing its live orchestrator, or re-opening it if dormant.
2. **Open local** (`l`) — a directory navigator (the existing path input + tab-completion) to pick a local repo; a git repo keys by its remote, a plain dir by the `local:` fallback.
3. **Open via git** (`g`) — prompt for a git URL; warden **clones into `~/.warden/workspace/<project>`** (disambiguating a name collision, e.g. `<host>-<org>-<repo>`), then treats it as a local project. An existing clone of the same remote is reused.

**Opening a project auto-spawns its orchestrator.** On open, warden spawns the project's single orchestrator agent, enforcing **one-orchestrator-per-project** — if one already anchors that project key it is **focused** in the agent pane rather than duplicated (the same invariant [`warden collaborate group join`](/warden/multi-agent/collaboration-groups/) uses). The project then appears in the Projects frame with its orchestrator as its top-level agent, ready to delegate. `Esc` returns the pane to the Projects view.

The recent-projects list persists in a lean project store (roster-style records — never transcripts), so it survives daemon restarts. Open Project and collaboration-group membership are distinct but share the one-orchestrator invariant and the project key.

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

## macOS: the Option key

The global rotation keys are `Alt`-based (`M-t`/`M-a` and their `Shift` reverses). On macOS the `Alt` key is **Option**, but by default **Terminal.app and iTerm2 do not send Option-combos as Meta** — pressing `Option+a` inserts a special character (`å`) rather than the `ESC`+`a` that tmux needs, so the `Option+…` rotation never fires. You have two ways around this:

- **Use the prefix fallback (nothing to configure).** Press `Ctrl-b` (the tmux prefix) then `t`/`a` — add `Shift` for reverse. It runs the exact same rotation and works on any terminal, because `Ctrl-b` is a plain control byte every emulator sends.
- **Or make Option behave as Meta**, then `Option+t`/`a` work directly:
  - **Terminal.app** — Settings → Profiles → Keyboard → check **"Use Option as Meta key."**
  - **iTerm2** — Settings → Profiles → Keys → set **Left Option key: Esc+.**

This only affects the `Alt`/`Option` rotation shortcuts; every other cockpit key (including pane-focus `Alt+←/→/↑/↓`, which many terminals send fine) is unaffected.

## Requirements

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes, and there is no single-pane fallback if tmux isn't installed. From a plain terminal it builds its own tmux session and attaches. From **inside an existing tmux session** (where a plain attach would nest), warden detects `$TMUX` and lays the cockpit out as a **native tmux window** in your *current* session instead — a leaner two-pane layout (control + agent, no terminal pane) that uses your own tmux keybindings, copy-mode, and resizing; `q` closes only the cockpit window. In native-window mode the terminal features (default terminal, `t`, `Enter` on a terminal, `M-t`) degrade to a status hint. Force the native window with `warden tui --tmux-native`, or force the classic three-pane own-session cockpit with `env -u TMUX warden tui`. The control pane polls the daemon about once a second, so the daemon must be running before you open the TUI.
