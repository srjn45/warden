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
| **daemon** | The single long-running process. Owns MongoDB, serves a loopback REST API on `127.0.0.1:8765`, and runs a background poller that keeps each agent's status and subject fresh. | Once, in the background (usually via launchd). |
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
tmux -V              # every agent lives in a tmux window
git --version        # worktree creation/cleanup
docker ps            # MongoDB runs in a container
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
make mongo-up        # start MongoDB first
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

- A new agent got an ID like `agent-a1b2` and its own working directory at
  `~/agentctl-agents/agent-a1b2/`.
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

Just pass a quoted prompt. The agent runs in a fresh per-agent directory and
**assumes no repository** — include any repo/context you need in the prompt
itself.

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
Open the live two-pane terminal cockpit (see §7). Bare `agentctl` with no
subcommand does the same thing.

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

A live two-pane cockpit (Bubble Tea). The **left pane** lists every agent with
a busy/idle badge and its current subject; the **right pane** shows the
selected agent's live output and event history. It polls the daemon about once
a second, so the daemon must be running.

| Key | Action |
|---|---|
| `↑`/`↓` or `j`/`k` | Move selection |
| `tab` | Focus the output viewport (scroll with `↑`/`↓`/`PgUp`/`PgDn`); `tab`/`esc` to leave |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `s` | Send a message to the selected agent — `enter` to send, `esc` to cancel |
| `a` | Attach — hands off to the agent's tmux session; returns to the TUI on detach |
| `x` | Terminate the selected agent — confirm with `y`; if it has uncommitted/unpushed work, press `X` to force |
| `?` | Toggle help |
| `q` | Quit |

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
`~/.claude/settings.json`. It posts `SessionStart`, `Notification`, `Stop`, and
`SubagentStop` events to the daemon.

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
| `AGENTCTL_MONGO_URI` | `mongodb://localhost:27017` | MongoDB connection URI |
| `AGENTCTL_DB` | `agentctl` | MongoDB database name |
| `AGENTCTL_WORKDIR` | `~/agentctl-agents` | Base dir for prompt-spawned agents; each gets `~/agentctl-agents/<id>/` |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects |

```sh
export AGENTCTL_WORKDIR=/path/to/your/agents
```

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
| `healthz` fails / daemon won't start | MongoDB down. `make mongo-up`, then check `/tmp/agentctl.daemon.err`. |
| New agent stuck at `classifying…` / type is `other` | `claude` not on the daemon's PATH. Type falls back to `other`; functionality is otherwise fine. |
| `SUBJECT` stays empty | Poller hasn't refreshed yet (it's throttled and only runs when pane content changes), or `CLAUDE_PROJECTS_DIR` is wrong. |
| `pr-review needs --pr or --branch` | pr-review requires one of those flags. |
| `done` refuses to clean up | The worktree has uncommitted or unpushed work — the guard is protecting it. Commit/push, or use `--force`. |
| Status never updates live | Hooks not wired into `~/.claude/settings.json` (§9). The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents run in `~/agentctl-agents/<id>/` and assume no repo — put repo context in the prompt, or use `--type ... --repo`. |
