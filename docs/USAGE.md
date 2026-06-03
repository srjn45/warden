# Using agentctl

A practical, task-oriented guide to running `agentctl` day to day. For build,
install, and contributor details see the [README](../README.md); this document
focuses on **how to use the tool once it's installed**.

> `alias agents=agentctl` is handy — every command below works under either name.

---

## 1. What agentctl is (the mental model)

`agentctl` lets you run many **Claude Code agent sessions** in parallel and
watch them from one place. Each agent is a real `claude` process running inside
its own detached **tmux** window. You spawn agents, watch what they're doing,
talk to them, and tear them down — without juggling terminals by hand.

One binary wears three hats:

| Face | What it is | You run it… |
|---|---|---|
| **daemon** | The single long-running process. Owns the on-disk session store, serves a loopback REST API on `127.0.0.1:8765`, and runs a background poller that keeps each agent's status and subject fresh. | Once, in the background (usually via launchd). |
| **CLI client** | `ls`, `status`, `start`, `done`, `attach`, `send`, `tail`, `tui` — thin HTTP clients that talk to the daemon. | Whenever you want to act on agents. |
| **MCP server** | `agentctl mcp` — a stdio bridge so an *orchestrator* Claude session can manage agents through tool calls. | Wired into a Claude session's MCP config. |

Everything flows through the daemon, so **the daemon must be running** before
any other command will work.

### The lifecycle of an agent

```
start ──▶ spawning ──▶ working ⇄ idle ⇄ waiting_for_input ──▶ done
                                                      └─▶ errored / orphaned
```

Status is driven by Claude Code lifecycle hooks (see §9) plus the daemon's
poller. You don't set it manually.

---

## 2. Prerequisites check

Before anything works, confirm these are on your PATH and running:

```sh
claude --version     # the agent runtime
tmux -V              # every agent lives in a tmux window (≥ 3.1 for the cockpit)
git --version        # worktree creation/cleanup
gh --version         # only needed for pr-review agents
curl -s localhost:8765/healthz   # → {"status":"ok"} means the daemon is up
```

If `healthz` doesn't return `ok`, start the daemon — see §3.

---

## 3. Starting the daemon

The daemon is the engine. Pick one of:

**Recommended — launchd (auto-start at login, restarts on crash):**

```sh
cp deploy/com.srajanpathak.agentctl.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist
```

**Manual (for debugging — runs in the foreground):**

```sh
agentctl daemon      # or: agentctl daemon --addr 127.0.0.1:9000
```

Verify and inspect logs:

```sh
curl -s localhost:8765/healthz   # {"status":"ok"}
tail -f /tmp/agentctl.daemon.log # stdout  (launchd)
tail -f /tmp/agentctl.daemon.err # stderr  (launchd)
```

Stop the launchd service:

```sh
launchctl unload ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist
```

---

## 4. Quickstart — your first agent

The fastest path is **prompt mode**: give a plain-English task and let
agentctl handle the rest. No repo, no flags.

```sh
agentctl start "review the auth module for security issues"
# spawned agent-a1b2 (classifying…) — attach with `agentctl attach agent-a1b2`
```

What just happened:

- A new agent got an ID like `agent-a1b2` and is launched in the directory you
  ran the command from (your "master shell" cwd) — no per-agent directory is
  created.
- It's running `claude` on your prompt inside a tmux window.
- The type shows as `classifying…` for a moment, then the daemon labels it
  (e.g. `analysis`) automatically.

Now watch and interact:

```sh
agentctl ls                         # see it in the list
agentctl status agent-a1b2          # full detail + event history
agentctl tail agent-a1b2            # recent terminal output
agentctl send agent-a1b2 "also check the session cookie handling"
agentctl attach agent-a1b2          # drop into its terminal (Ctrl-b d to detach)
agentctl done agent-a1b2            # tear it down when finished
```

That's the whole loop. Everything else is variations on it.

---

## 5. Two ways to spawn

### Prompt mode (default — no worktree, auto-typed)

Just pass a quoted prompt. The agent launches in your current directory (or the
`--dir` you pass) and **assumes no worktree** — it operates directly on whatever
is in that directory. Use `--dir` to point it elsewhere.

```sh
agentctl start "investigate why the nightly build is flaky"
agentctl start "summarize the changes in /path/to/repo since last Friday"
```

- **Type is auto-assigned** shortly after spawn (the daemon asks `claude -p` to
  classify the prompt; falls back to `other` if `claude` isn't available).
- **Subject is auto-generated** — a ≤8-word phrase summarizing current work,
  seeded from the prompt and refreshed by the poller.

### Managed worktree mode (`--type`)

When the work belongs to a real repo — especially a development branch tied to
a ticket — pass `--type`. This is what creates and manages a git worktree.

```sh
# Development branch for a Jira ticket (new worktree + new branch):
agentctl start PROJ-350 --type development

# PR review (fresh worktree, runs `gh pr checkout`):
agentctl start --type pr-review --pr 1234

# Spike/analysis with an opt-in scratch worktree:
agentctl start --type spike --worktree

# Buildkite debug — no worktree, runs in the current repo:
agentctl start --type buildkite-debug

# Be explicit about repo and branch:
agentctl start PROJ-350 --type development --repo /path/to/repo --branch my-branch
```

**Type → worktree behavior:**

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | Creates `.worktrees/<ticket>` on a branch named after the ticket |
| `pr-review` | yes (PR branch) | Runs `gh pr checkout <PR>` inside it. **Requires `--pr` or `--branch`** |
| `analysis` | opt-in `--worktree` | Runs in the repo by default |
| `spike` | opt-in `--worktree` | Same as analysis |
| `buildkite-debug` | no | Runs in the repo root |
| `test-run` | no | Runs in the repo root |
| `env-test` | no | Runs in the repo root |
| `other` | no | Catch-all; also where unrecognized type strings land |

Notes:
- If a worktree for the ticket already exists on disk, the spawn **adopts** it
  rather than erroring.
- Every agent runs `claude --dangerously-skip-permissions` — permission
  prompts are suppressed, but the `Notification` hook still records them as
  events you can see in `status`.

---

## 6. Command reference

All commands accept `--addr` to point at a non-default daemon (overrides
`AGENTCTL_ADDR`). `<TICKET>` is the agent ID — a Jira key for managed agents,
or an `agent-xxxx` ID for prompt-spawned ones.

### `agentctl` / `agentctl tui`
Open the tmux-composited cockpit (see §7). Bare `agentctl` with no
subcommand does the same thing. Pass `--classic` to use the legacy single-pane view.

### `agentctl start [TICKET|"<prompt>"] [flags]`
Spawn an agent. Prompt mode if no `--type`; managed-worktree mode otherwise.

| Flag | Meaning |
|---|---|
| `--type` | `development\|analysis\|spike\|pr-review\|buildkite-debug\|test-run\|env-test\|other`. Omit for prompt mode. |
| `--repo` | Repo path (default: current directory; managed mode only). |
| `--branch` | New branch (development) or checkout target (pr-review). |
| `--pr` | PR number/URL (pr-review). |
| `--worktree` | Create a scratch worktree for analysis/spike. |

### `agentctl ls`
List all active sessions: `ID  TYPE  STATUS  AGE  DIR  SUBJECT`.
`DIR` is the base name of the working directory; `SUBJECT` is empty until the
first poller refresh; `TYPE` shows `…` while a prompt agent is still being
classified.

### `agentctl status <TICKET>`
Full detail for one session: id, type, ticket, status, repo, workdir,
worktree, branch, pr, subject, last-updated, and the full event timeline.

### `agentctl tail <TICKET> [--lines N]`
Print the recent terminal output of the agent's claude session
(default 200 lines).

```sh
agentctl tail PROJ-350 --lines 80
```

### `agentctl send <TICKET> <message...>`
Type a message into the agent's claude session and press Enter — exactly as if
you'd typed it at the prompt.

```sh
agentctl send PROJ-350 "run the unit tests and fix any failures"
```

### `agentctl adopt [--session-id <uuid>] [--dir <path>]`
Register an existing Claude session into agentctl.

- **Plain shell** — finds the newest Claude conversation for the directory and
  resumes it under a new tmux session (`claude --resume`).
- **Inside tmux** — registers the current tmux session live without relaunching
  claude.

```sh
agentctl adopt                          # newest session for cwd, resume under tmux
agentctl adopt --session-id <uuid>      # pick a specific Claude conversation
agentctl adopt --dir /path/to/project   # target a different directory
```

### `agentctl attach <TICKET>`
Hand your terminal to the agent's tmux session interactively. Detach with the
tmux prefix-then-`d` (default `Ctrl-b d`) to leave the agent running.

### `agentctl done <TICKET> [--force] [--hard]`
Tear down: kill tmux, prune the worktree and branch, archive the record.

The cleanup is **guarded** — it refuses to remove a worktree with uncommitted
changes or unpushed commits.

```sh
agentctl done PROJ-350          # guarded (safe default)
agentctl done PROJ-350 --force  # bypass the git guard
agentctl done PROJ-350 --hard   # hard-delete the doc instead of archiving
```

### `agentctl daemon [--addr ADDR]`
Run the hub (HTTP API + poller). Normally launchd's job; run by hand to debug.

### `agentctl mcp [--addr ADDR]`
Run the MCP stdio server (see §8).

---

## 7. The terminal UI (TUI)

```sh
agentctl tui     # or just: agentctl
```

### The tmux-composited cockpit (default)

`agentctl tui` builds a **tmux-composited cockpit** — a dedicated tmux session
with three panes laid out like this:

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

**Top-left — agents list.** Lists every agent with a busy/idle badge and its
current subject. Scroll through the list with `↑`/`↓` or `j`/`k` — browsing
does not disturb whatever is open in the right pane. Press `Enter` on a
highlighted agent to open it in the right pane.

| Key | Action |
|---|---|
| `↑`/`↓` or `j`/`k` | Move selection (right pane is unaffected) |
| `Enter` | Open the selected agent in the right detail pane |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `s` | Send a message to the selected agent — `enter` to send, `esc` to cancel |
| `a` | Attach — hands the whole client to the agent's tmux session. Press **`Ctrl-b Enter`** to return to the dashboard (a hint flashes on attach). |
| `x` | Terminate the selected agent — confirm with `y`, cancel with `n`/`esc` |
| `?` | Toggle help |
| `q` | Quit and tear down the whole cockpit |

> **Getting back from an agent.** Attaching moves your single tmux client onto
> the agent's session (tmux can't nest an attach), so use **`Ctrl-b Enter`** to
> jump back to the dashboard — not `Ctrl-b d`. `Ctrl-b d` still works but it
> *detaches* the cockpit to the background rather than returning to it; the
> cockpit survives (it's reaped on your next `agentctl tui`), so an accidental
> detach no longer destroys your dashboard. Only `q` tears it down.

**Bottom-left — master Claude.** A live, interactive `claude` session embedded
directly in the cockpit. It is wired to the `agentctl` MCP server, so you can
talk to it naturally to manage or monitor the whole fleet — *"triage all my
agents and tell me which are stuck"*, *"tell PROJ-350 to run the tests"*,
or anything else Claude Code can do.

The `agentctl` MCP server is registered when the installer runs
`claude mcp add agentctl --scope user -- agentctl mcp`. If it isn't registered,
the master can still drive the fleet using the `agentctl` CLI directly.

> **The master is ephemeral.** It starts fresh every time you open the cockpit
> and dies when you quit it. Persisting the master across sessions (so it can
> survive long-running orchestration and resume context on next launch) is a
> planned future enhancement — see
> `docs/superpowers/specs/2026-06-03-agentctl-tui-master-pane-design.md`.

**Right (full height) — live agent detail pane.** When you press `Enter` on an
agent in the list, a live, interactive terminal of that agent's `claude` session
opens here — the same way the bottom-left master pane works. You can type
directly into the agent, read its output, and watch it respond in real time.
Scrolling the agents list with `↑`/`↓` or `j`/`k` does not replace this pane,
so an agent you're actively working with is never interrupted by casual
browsing. Press `Enter` again on a different agent to switch.

To move focus between panes without leaving the cockpit, use **Alt+←/→/↑/↓**
(no tmux prefix needed).

> **Caveats — nested tmux and Alt+Arrow navigation:**
> Because the right pane runs a tmux client nested inside the cockpit session,
> the normal tmux prefix (`Ctrl-b`) is ambiguous there and will be captured by
> the outer session. Use **Alt+Arrow** to move between panes instead. These
> bindings are applied tmux-server-wide, so they will also affect any other tmux
> sessions you have open on the same server. Requires **tmux ≥ 3.1**.

Each cockpit launch creates an independent tmux session (named
`agentctl-tui-<pid>`), so opening two terminals and running `agentctl tui` in
each gives you two separate cockpits, each with its own ephemeral master.

### Classic (single-pane) mode

```sh
agentctl tui --classic
```

Runs the original single-pane view: list on the left, a static detail panel on
the right, no embedded master — the same Bubble Tea app that existed before the
cockpit. Use it if you prefer a lighter view or need to script into a
non-interactive environment. The right-pane detail panel in this mode does not
embed a live agent session.

The cockpit **automatically falls back to `--classic`** in two situations:
- `tmux` is not installed.
- `agentctl tui` is launched from inside an existing tmux session (to avoid
  nesting sessions).

(There is no tmux-version detection — tmux ≥ 3.1 is a requirement of the
cockpit, not a fallback trigger; on an older tmux the cockpit may fail to build
its panes.)

The list pane polls the daemon about once a second, so the daemon must be
running regardless of which mode you use.

---

## 8. Orchestrating agents from another Claude session (MCP)

Register `agentctl mcp` as an MCP server in your *orchestrator* Claude session
so it can manage agents via tool calls. Add to your MCP config (e.g.
`~/.claude/claude_desktop_config.json` or a project `.claude/mcp.json`):

```json
{
  "mcpServers": {
    "agentctl": {
      "command": "agentctl",
      "args": ["mcp"],
      "env": { "AGENTCTL_ADDR": "127.0.0.1:8765" }
    }
  }
}
```

Tools exposed:

| Tool | Does |
|---|---|
| `list_agents` | List all agents with status, workdir, subject |
| `get_agent` | Full detail (status, workdir, subject, events, worktree) for one |
| `spawn_agent` | Spawn a new agent of a given type |
| `adopt_agent` | Register an existing Claude session: resume newest-for-dir under tmux, or live-register a running tmux session |
| `send_to_agent` | Type a message into a specific agent's claude session |
| `get_agent_output` | Recent terminal output of a specific agent |
| `cleanup_agent` | Tear down an agent and archive its record |

Then just talk to the orchestrator naturally:

- *"What is PROJ-350 doing?"* → `get_agent`
- *"Tell PROJ-343 to run the tests"* → `send_to_agent`
- *"List all my agents"* → `list_agents`
- *"Spawn a buildkite-debug agent in /path/to/repo"* → `spawn_agent`
- *"Clean up PROJ-350 when it's done"* → `cleanup_agent`

---

## 9. The Claude Code hooks (status in real time)

For statuses to update live (rather than only on poll), wire the lifecycle hook
into Claude Code by merging `hooks/settings.snippet.json` into
`~/.claude/settings.json`. It posts `SessionStart`, `Notification`, `Stop`,
`SubagentStop`, and `SessionEnd` events to the daemon. `SessionEnd` (claude
exited) marks the session **done** — a terminal status the poller leaves
untouched, so a finished agent won't drift to `orphaned` when its tmux session
later goes away.

The hook **fails soft**: it never blocks or errors an agent, even if the daemon
is down or the session is unknown. (It also no-ops outside tmux, since it uses
the tmux session name as the agent ID.)

---

## 10. The web dashboard

The daemon embeds a React dashboard and serves it at the same address as the
API:

```sh
agentctl daemon
open http://localhost:8765
```

It mirrors the CLI/TUI: a live agent list over SSE, busy/idle badges, a
**+ New agent** prompt box, a per-agent detail panel with live output and a
send-message box, and a **Terminate** button that surfaces the same git guard
(with **Force** and an optional hard-delete) when there's unsaved work.
Browsers can't run `tmux attach`, so the detail panel just shows the
`agentctl attach <id>` command to copy into a terminal.

> The UI is baked into the binary at build time. After changing anything under
> `web/`, rebuild (`make release`, or `make ui` for the frontend only) and
> restart the daemon. For live UI iteration, run `agentctl daemon` and
> `make ui-dev` in parallel and open `http://localhost:4321`.

---

## 11. Configuration

Set via environment variables (or override the daemon address per-command with
`--addr`):

| Variable | Default | Description |
|---|---|---|
| `AGENTCTL_ADDR` | `127.0.0.1:8765` | Daemon listen/connect address |
| `AGENTCTL_DATA_DIR` | `~/.agentctl` | Directory for session JSON files (`sessions/`, `closed/`) and prompt files (`prompts/`) |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects |

---

## 12. Status values you'll see

| Status | Meaning |
|---|---|
| `spawning` | Session is being created |
| `working` | Actively doing work |
| `waiting_for_input` | Paused on a question/notification — `send` it an answer |
| `idle` | Alive but not currently working |
| `done` | Finished |
| `errored` | Hit an error |
| `orphaned` | The daemon lost track of its tmux session |

---

## 13. Typical workflows

**Ad-hoc investigation (prompt mode):**
```sh
agentctl start "find and fix the flaky test in the payments suite"
agentctl ls
agentctl tail <id>
agentctl send <id> "skip the integration tests for now"
agentctl done <id>
```

**Ticketed development (managed worktree):**
```sh
agentctl start PROJ-350 --type development     # worktree + branch
agentctl status PROJ-350
agentctl attach PROJ-350                       # jump in when needed
agentctl done PROJ-350                          # guarded teardown
```

**Reviewing a PR:**
```sh
agentctl start --type pr-review --pr 1234
agentctl tail prreview-... 
agentctl done prreview-...
```

---

## 14. Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Any command hangs or errors connecting | Daemon not running. `curl localhost:8765/healthz`; start it (§3). |
| `healthz` fails / daemon won't start | Data dir not writable. Check `AGENTCTL_DATA_DIR` (default `~/.agentctl`) and `/tmp/agentctl.daemon.err`. |
| New agent stuck at `classifying…` / type is `other` | `claude` not on the daemon's PATH. Type falls back to `other`; functionality is otherwise fine. |
| `SUBJECT` stays empty | Poller hasn't refreshed yet (it's throttled and only runs when pane content changes), or `CLAUDE_PROJECTS_DIR` is wrong. |
| `pr-review needs --pr or --branch` | pr-review requires one of those flags. |
| `done` refuses to clean up | The worktree has uncommitted or unpushed work — the guard is protecting it. Commit/push, or use `--force`. |
| Status never updates live | Hooks not wired into `~/.claude/settings.json` (§9). The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents launch in your current directory — `cd` to the right place first, or pass `--dir <path>`. |
