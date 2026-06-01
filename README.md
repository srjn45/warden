# agentctl

A single Go binary that spawns, monitors, and tears down Claude Code agent sessions of different task types — creating a git worktree only for the types that need one — backed by a local daemon and MongoDB.

One binary, multiple faces: `agentctl daemon` is the single MongoDB writer, serving a loopback REST API and running a background poller. `agentctl ls|status|start|done|attach|send|tail` are thin HTTP clients to the daemon. `agentctl mcp` is a stdio MCP server that bridges MCP tool calls to the same REST API, enabling an orchestrator Claude session to query agents and talk to a specific running agent.

```
alias agents=agentctl
```

---

## Prerequisites

- **Go 1.22+** — to build the binary
- **tmux** — every agent session runs in a detached tmux window
- **git** — worktree creation and guarded cleanup
- **Docker** — runs the local MongoDB instance via Docker Compose
- **Claude Code** (`claude` on PATH) — the agent runtime launched in each session
- **`gh`** (GitHub CLI) — required for `pr-review` sessions to check out the PR branch

---

## Build

```sh
make build           # produces bin/agentctl
make mongo-up        # starts mongodb:7 on localhost:27017
```

---

## Install the daemon as a launchd service (auto-start)

Copy the plist into `~/Library/LaunchAgents` and load it:

```sh
cp deploy/com.srajanpathak.agentctl.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist
```

The daemon starts automatically at login and restarts on crash (`KeepAlive = true`). It listens on `127.0.0.1:8765` by default.

Check health:

```sh
curl -s localhost:8765/healthz
# {"status":"ok"}
```

Logs:
- stdout: `/tmp/agentctl.daemon.log`
- stderr: `/tmp/agentctl.daemon.err`

To stop:

```sh
launchctl unload ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist
```

---

## Wire in the Claude Code hooks

The hook script posts lifecycle events (`SessionStart`, `Notification`, `Stop`, `SubagentStop`) to the daemon so it can update agent status in real time without polling.

Merge `hooks/settings.snippet.json` into `~/.claude/settings.json`:

```sh
# If ~/.claude/settings.json doesn't exist yet:
cp hooks/settings.snippet.json ~/.claude/settings.json

# If it already exists, add the "hooks" key from settings.snippet.json
# into the root of your existing settings.json object.
```

The snippet references the installed hook location:
```
~/workspace/personal/agentctl/hooks/agentctl-hook.sh
```

The hook fails soft — it never blocks or errors the agent, even if the daemon is down or the session is unknown.

---

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `AGENTCTL_ADDR` | `127.0.0.1:8765` | Daemon listen address |
| `AGENTCTL_MONGO_URI` | `mongodb://localhost:27017` | MongoDB connection URI |
| `AGENTCTL_DB` | `agentctl` | MongoDB database name |

All variables can also be overridden with `--addr` on any command.

---

## Task types and the `--type` flag

Every `agentctl start` requires `--type`. The type controls whether a git worktree is created and determines how the session is set up.

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | Creates `.worktrees/<ticket>` on a new branch named after the ticket |
| `pr-review` | yes (PR branch) | Detached worktree; runs `gh pr checkout <PR>` inside it. Requires `--pr` or `--branch` |
| `analysis` | opt-in (`--worktree`) | Runs in the repo by default; pass `--worktree` to get a scratch branch |
| `spike` | opt-in (`--worktree`) | Same as analysis |
| `buildkite-debug` | no | Runs directly in the repo root |
| `test-run` | no | Runs directly in the repo root |
| `env-test` | no | Runs directly in the repo root |
| `other` | no | Catch-all; also used for unrecognized type strings |

Every agent runs `claude --dangerously-skip-permissions` — permission prompts are suppressed; the `Notification` hook still records them as events in the session doc.

If a worktree for the ticket already exists on disk, the spawn adopts it (reattaches claude to the existing branch) instead of erroring.

---

## Command reference

### `agentctl start [TICKET] --type <TYPE>`

Spawn a new agent session. `TICKET` is optional for types without a worktree; for `development`/`pr-review` it becomes the session id and the branch name.

```sh
# Development agent for a Jira ticket:
agentctl start PROJ-350 --type development

# PR review — checks out the PR branch in a fresh worktree:
agentctl start --type pr-review --pr 1234

# Buildkite debug — no worktree, runs in current directory:
agentctl start --type buildkite-debug

# Spike with an optional scratch worktree:
agentctl start --type spike --worktree

# Point at a specific repo and branch:
agentctl start PROJ-350 --type development --repo /path/to/repo --branch my-branch
```

Flags:
- `--type` — task type (required)
- `--repo` — repo path (default: current directory)
- `--branch` — new branch name (development) or checkout target (pr-review)
- `--pr` — PR number or URL (pr-review only)
- `--worktree` — opt-in worktree for analysis/spike

### `agentctl ls`

List all active agent sessions with their type, status, and last event.

```sh
agentctl ls
# ID                TYPE         STATUS    AGE   DETAIL
# PROJ-350    development  working   2m    …
# prreview-a1b2     pr-review    idle      5m    …
```

### `agentctl status <TICKET>`

Show full detail for one session: worktree, branch, PR, all events.

```sh
agentctl status PROJ-350
```

### `agentctl attach <TICKET>`

Attach your terminal to the agent's tmux session interactively.

```sh
agentctl attach PROJ-350
```

### `agentctl done <TICKET>`

Tear down the agent: kill tmux, prune worktree and branch (guarded), archive the record.

The cleanup is guarded: it will refuse to remove a worktree that has uncommitted changes or unpushed commits. Use `--force` to override.

```sh
agentctl done PROJ-350          # guarded
agentctl done PROJ-350 --force  # bypass git guard
agentctl done PROJ-350 --hard   # hard-delete the doc instead of archiving
```

### `agentctl send <TICKET> <message...>`

Type a message into the agent's claude session and press Enter.

```sh
agentctl send PROJ-350 "run the unit tests and fix any failures"
```

### `agentctl tail <TICKET>`

Print the recent terminal output of the agent's claude session.

```sh
agentctl tail PROJ-350
agentctl tail PROJ-350 --lines 80
```

### `agentctl daemon`

Run the daemon (HTTP API + background poller). Normally managed by launchd; run manually for debugging.

```sh
agentctl daemon
agentctl daemon --addr 127.0.0.1:9000
```

### `agentctl mcp`

Run the MCP stdio server so an orchestrator Claude session can manage agents via tool calls.

```sh
agentctl mcp
agentctl mcp --addr 127.0.0.1:8765
```

Tools exposed: `list_agents`, `get_agent`, `spawn_agent`, `send_to_agent`, `get_agent_output`, `cleanup_agent`.

---

## Orchestrator (MCP)

Register `agentctl mcp` as an MCP server in your orchestrator Claude session's MCP config (e.g. `~/.claude/claude_desktop_config.json` or the project-level `.claude/mcp.json`):

```json
{
  "mcpServers": {
    "agentctl": {
      "command": "agentctl",
      "args": ["mcp"],
      "env": {
        "AGENTCTL_ADDR": "127.0.0.1:8765"
      }
    }
  }
}
```

Once registered, the orchestrator session can call these tools directly:

| Tool | Description |
|---|---|
| `list_agents` | List all active agents and their current status |
| `get_agent` | Get full detail (status, events, worktree) for one agent |
| `spawn_agent` | Spawn a new agent of a given task type |
| `send_to_agent` | Type a message into a specific agent's claude session |
| `get_agent_output` | Return the recent terminal output of a specific agent |
| `cleanup_agent` | Tear down an agent and archive its record |

Example orchestrator prompts:

- "What is PROJ-350 doing?" — calls `get_agent` to fetch current status and events
- "Tell PROJ-343 to run the tests" — calls `send_to_agent` with `"run the tests"`
- "List all my agents" — calls `list_agents`
- "Spawn a buildkite-debug agent in /path/to/repo" — calls `spawn_agent`
- "Clean up PROJ-350 when it's done" — calls `cleanup_agent`

---

## Typical workflow

```sh
# 1. Start Mongo and the daemon (once — then managed by launchd)
make mongo-up
make run-daemon   # or: launchctl load ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist

# 2. Spawn an agent for a ticket
agentctl start PROJ-350 --type development

# 3. Watch what it's doing
agentctl ls
agentctl status PROJ-350

# 4. Drop into its terminal if needed
agentctl attach PROJ-350

# 5. Clean up when done
agentctl done PROJ-350
```

---

## Development

```sh
make build          # go build -o bin/agentctl ./cmd/agentctl
make test           # go test ./...
make lint           # go vet ./...
make mongo-up       # start local MongoDB container
make mongo-down     # stop it
make run-daemon     # build + start daemon locally
```

Tests that need Docker (the Mongo integration suite) are skipped in `-short` mode:

```sh
go test -short ./...   # fast, no Docker needed
```
