# agentctl

A single Go binary that spawns, monitors, and tears down Claude Code agent sessions of different task types — creating a git worktree only for the types that need one — backed by a local daemon and a file-based JSON store (no database to run).

One binary, multiple faces: `agentctl daemon` is the single writer to the on-disk session store, serving a loopback REST API and running a background poller. `agentctl ls|status|start|done|attach|send|tail` are thin HTTP clients to the daemon. `agentctl mcp` is a stdio MCP server that bridges MCP tool calls to the same REST API, enabling an orchestrator Claude session to query agents and talk to a specific running agent.

```
alias agents=agentctl
```

> **New here?** See [docs/USAGE.md](docs/USAGE.md) for a task-oriented guide to running agents day to day. The sections below cover build, install, and contributor setup.

---

## Prerequisites

- **Go 1.22+** — to build the binary
- **tmux** — every agent session runs in a detached tmux window
- **git** — worktree creation and guarded cleanup
- **Claude Code** (`claude` on PATH) — the agent runtime launched in each session
- **`gh`** (GitHub CLI) — required for `pr-review` sessions to check out the PR branch

---

## Build

```sh
make build           # produces bin/agentctl
```

---

## Install the daemon as a launchd service (auto-start)

Install with the script — it builds the release, installs the binary to
`~/.local/bin/agentctl`, renders and loads the launchd plist, links the Claude
skill, and registers the MCP server:

```sh
./scripts/install.sh        # or: make install
```

The daemon then starts automatically at login and restarts on crash
(`KeepAlive = true`), listening on `127.0.0.1:8765` by default.

> `~/.local/bin` must be on your `PATH` to run `agentctl` from the shell — the
> installer warns if it isn't.

### Stop macOS "agentctl would like to access…" prompts (optional, macOS)

The launchd daemon is the macOS TCC *responsible process* for the agents it
spawns and for its own directory picker, so reads of protected folders
(Downloads, Documents, Desktop, the Music/media library) surface as *"agentctl
would like to access…"* prompts. Granting Full Disk Access once silences them —
but macOS ties the grant to the binary's code identity, and an unsigned Go
binary gets a new identity on every rebuild, which brings the prompts back.

Run the one-time setup to give the binary a **stable** self-signed identity:

```sh
./scripts/codesign-setup.sh   # creates a self-signed code-signing cert (once)
./scripts/install.sh          # reinstall so the binary is signed
```

Then grant access once: **System Settings → Privacy & Security → Full Disk
Access → "+"** and add `~/.local/bin/agentctl`. Because the signing identity is
stable, the grant survives future rebuilds. (`install.sh`/`reinstall.sh` sign
automatically when the cert exists; without it they warn and leave the binary
unsigned.)

**Redeploy after a code change** (replaces `make release && ./bin/agentctl daemon`):

```sh
./scripts/reinstall.sh             # rebuild UI + binary, redeploy, restart
./scripts/reinstall.sh --no-build  # redeploy the existing build only
# or: make reinstall  /  make reinstall NO_BUILD=1
```

**Uninstall** (stops and removes the service, binary, skill link, and MCP
registration; **preserves** your session store at `~/.agentctl` and the logs):

```sh
./scripts/uninstall.sh                 # or: make uninstall
./scripts/uninstall.sh --keep-binary   # leave ~/.local/bin/agentctl in place
```

Logs:

- stdout: `/tmp/agentctl.daemon.log`
- stderr: `/tmp/agentctl.daemon.err`

> **Notifications:** off by default. When enabled with `AGENTCTL_NOTIFY=on`, the daemon posts a macOS notification when an agent enters `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`. These appear only when the daemon runs in your GUI login session (a terminal, or a launchd **user agent**); a headless/system daemon logs them instead.

---

## Wire in the Claude Code hooks

The hook script posts lifecycle events (`SessionStart`, `Notification`, `Stop`, `SubagentStop`, `SessionEnd`) to the daemon so it can update agent status in real time without polling. `SessionEnd` marks the session **done** (terminal) when claude exits.

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
| `AGENTCTL_DATA_DIR` | `~/.agentctl` | Directory for session JSON files (`sessions/`, `closed/`) |
| `AGENTCTL_WORKDIR` | `~/agentctl-agents` | Where the per-agent prompt file is stored (keyed by agent id). It is **not** where the agent runs — prompt-spawned agents launch in the caller's current directory |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Root of Claude Code transcript directories; used by the poller to read agent transcripts when generating subjects |
| `AGENTCTL_NOTIFY` | `off` | macOS desktop notifications when an agent needs attention (`on`/`1`/`true` to enable) |
| `AGENTCTL_APPROVALS` | `off` | Enables the approvals inbox: the daemon parses recognized Claude Code tool-permission prompts and surfaces one-click answer buttons. Web AttentionQueue shows option buttons; TUI gains a pinned **⏳ Approvals** row (press `i` to focus, answer with number keys). Unrecognized prompts always fall back to attach. Enable with `1`/`on`/`true` |

All variables can also be overridden with `--addr` on any command.

---

## Prompt-spawn (no repo, auto-typed)

The simplest way to create an agent is to give it a plain prompt and let the system figure out the rest:

```sh
agentctl start "review the auth module for security issues"
# spawned agent-a1b2 (classifying…) — attach with `agentctl attach agent-a1b2`
```

In the web GUI, **+ New agent** opens a single prompt textarea — no type or repo fields.

**How it works:**

- **Runs in the caller's directory.** Prompt-spawned agents run `claude --dangerously-skip-permissions '<prompt>'` (or `--permission-mode acceptEdits` with `--supervised`) in the directory you invoked `start` from (or the `--dir` you pass) — no per-agent directory is created. Point it elsewhere with `--dir`, and include any extra repo context in the prompt itself.
- **Type is auto-assigned.** Shortly after creation the daemon classifies the prompt with `claude -p` and updates the type label. It appears as "classifying…" until then. Requires `claude` on the daemon's `PATH`; falls back to `other` if unavailable.
- **Subject is auto-generated.** Each agent has a one-line subject summarizing what it is currently working on. It is seeded from the first words of the prompt at spawn, then refreshed periodically by the poller: the poller reads the agent's Claude Code transcript (looked up by `CLAUDE_PROJECTS_DIR`) or, if no transcript is found, captures the tmux pane, then asks `claude -p` for an ≤8-word phrase. Refreshes are throttled and only run when the pane content has changed.
- **Managed worktrees still available.** `agentctl start TICKET --type development --repo …` is unchanged — see the section below.

To launch an agent in a directory other than your current one, pass `--dir`:

```sh
agentctl start "summarize recent changes" --dir /path/to/repo
```

---

## Task types and the `--type` flag

When you need a managed git worktree (e.g. a development branch tied to a Jira ticket), pass `--type`. The type controls whether a git worktree is created and determines how the session is set up.

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

By default every agent runs `claude --dangerously-skip-permissions` — permission prompts are suppressed and the agent runs fully autonomously; the `Notification` hook still records them as events in the session doc.

Pass `--supervised` to opt into a lighter permission mode (`--permission-mode acceptEdits`): file edits and common filesystem commands auto-approve, but other tools (bash writes, network calls, etc.) surface the numbered permission prompt — which the approvals inbox captures and lets you answer (web AttentionQueue one-click buttons / TUI `⏳ Approvals` row, when `AGENTCTL_APPROVALS` is on). A restored agent keeps its supervised setting.

If a worktree for the ticket already exists on disk, the spawn adopts it (reattaches claude to the existing branch) instead of erroring.

---

## Terminal UI

```sh
agentctl tui   # open the cockpit
agentctl       # bare invocation — same thing
```

`agentctl tui` (or bare `agentctl`) opens a **tmux-composited cockpit** — a dedicated tmux session with three panes: an agents list (top-left), an embedded interactive **master Claude** session wired to the `agentctl` MCP server (bottom-left), and a full-height live detail pane (right) that opens the selected agent's interactive `claude` session. Browse the list freely with `↑`/`↓` without disturbing the detail pane; press `Enter` to open an agent in it.

The cockpit **falls back to the legacy single-pane view** (`--classic`) when tmux isn't installed or when `agentctl tui` is launched from inside an existing tmux session. The classic view is the original [Bubble Tea](https://github.com/charmbracelet/bubbletea) app — a list on the left and a static detail panel on the right (no embedded sessions). Requires **tmux ≥ 3.1** for the cockpit.

The list pane polls the daemon about once a second. The daemon must be running (`agentctl daemon`) before opening the TUI.

**Keys (cockpit)**

| Key | Action |
|---|---|
| `↑` / `↓` or `j` / `k` | Move selection (detail pane is unaffected) |
| `Enter` | Open the selected agent in the right detail pane |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `s` | Send a message to the selected agent — `enter` to send, `esc` to cancel |
| `a` | Attach — hands the client to the agent's tmux session; press **`Ctrl-b Enter`** to return to the dashboard |
| `x` | Terminate the selected agent — confirm with `y`, cancel with `n`/`esc` |
| `?` | Toggle help overlay |
| `q` | Quit and tear down the cockpit |

Move focus between panes with **Alt+←/→/↑/↓** (no tmux prefix). See [docs/USAGE.md §7](docs/USAGE.md) for the full cockpit guide, caveats around nested tmux, and the classic-mode key list.

---

## Command reference

### `agentctl tui`

Open the live terminal cockpit. Also launched by bare `agentctl` with no subcommand.

```sh
agentctl tui
agentctl       # equivalent
```

See the [Terminal UI](#terminal-ui) section above for the full key reference.

### `agentctl start [TICKET|"<prompt>"] [--type <TYPE>]`

Spawn a new agent session. Two modes:

**Prompt mode** (no `--type`) — pass a quoted prompt; the type is assigned automatically:

```sh
agentctl start "review the auth module for security issues"
agentctl start "investigate why the nightly build is flaky"
```

**Managed worktree mode** (`--type` required) — creates a git worktree for the ticket:

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
- `--type` — task type; omit to use prompt mode (auto-typed)
- `--repo` — repo path (default: current directory; managed worktree mode only)
- `--branch` — new branch name (development) or checkout target (pr-review)
- `--pr` — PR number or URL (pr-review only)
- `--worktree` — opt-in worktree for analysis/spike
- `--supervised` — launch with `--permission-mode acceptEdits` instead of the default `--dangerously-skip-permissions`; risky tools prompt and the approvals inbox surfaces them (see `AGENTCTL_APPROVALS`)

### `agentctl ls`

List all active agent sessions with their type, status, working directory, and subject.

```sh
agentctl ls
# ID                TYPE         STATUS    AGE   DIR                SUBJECT
# PROJ-350    development  working   2m    PROJ-350     refactoring auth middleware
# prreview-a1b2     pr-review    idle      5m    prreview-a1b2      reviewing PR 1234
# agent-c3d4        …            working   1m    agent-c3d4         investigate flaky nightly build
```

`DIR` shows the base name of the agent's working directory. `SUBJECT` is the auto-generated one-line summary of what the agent is currently doing (empty until the first poller refresh).

Use `--json` for machine-readable output (a JSON array of full session objects; an empty fleet prints `[]`). Useful for scripts and for Claude driving the CLI:

```sh
agentctl ls --json
```

### `agentctl status <TICKET>`

Show full detail for one session: working directory, subject, worktree, branch, PR, all events.

```sh
agentctl status PROJ-350
```

Add `--json` to emit the full session as a single JSON object (including the `events` array):

```sh
agentctl status PROJ-350 --json
```

### `agentctl adopt [--session-id <uuid>] [--dir <path>]`

Register an existing Claude session into agentctl.

- **Plain shell** — finds the newest Claude conversation for the directory and resumes it under a new tmux session (`claude --resume`).
- **Inside tmux** — registers the current tmux session live without relaunching claude.

```sh
agentctl adopt                          # newest session for cwd, resume under tmux
agentctl adopt --session-id <uuid>      # pick a specific Claude conversation
agentctl adopt --dir /path/to/project   # target a different directory
```

### `agentctl attach <TICKET>`

Attach your terminal to the agent's tmux session interactively.

```sh
agentctl attach PROJ-350
```

### `agentctl done <TICKET>`

Terminate the agent (kill its tmux + claude session) **and** clear its stored record in one step. It does **not** remove the git worktree — that is a separate, explicitly-confirmed step (`remove-worktree`). Equivalent to `terminate` followed by `delete`.

```sh
agentctl done PROJ-350          # terminate + clear record (worktree kept)
agentctl done PROJ-350 --hard   # purge the record instead of archiving it
```

### `agentctl terminate <TICKET>`

Stop an agent: kill its tmux + claude session, but **keep** the record and worktree. This is the safe "stop this agent" default — it is reversible with `agentctl restore`.

```sh
agentctl terminate PROJ-350
```

### `agentctl restore <TICKET>`

Recreate and resume a lost/orphaned agent's tmux + claude session (`claude --resume`). Use only when the agent's tmux session is gone (status `orphaned`).

```sh
agentctl restore PROJ-350
```

### `agentctl delete <TICKET>`

Clear an agent's stored record (archives by default; `--hard` purges). Does not touch tmux or the worktree.

```sh
agentctl delete PROJ-350
agentctl delete PROJ-350 --hard
```

### `agentctl remove-worktree <TICKET>`

Remove an agent's git worktree and branch. **Destructive.** It refuses if the agent is still running (terminate it first) or if the worktree has uncommitted changes or unpushed commits — use `--force` to override the guard.

```sh
agentctl remove-worktree PROJ-350
agentctl remove-worktree PROJ-350 --force
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

Tools exposed: `list_agents`, `get_agent`, `spawn_agent`, `adopt_agent`, `send_to_agent`, `get_agent_output`, `terminate_agent`, `restore_agent`, `delete_agent`, `remove_worktree`.

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
| `list_agents` | List all active agents with their status, working directory, and subject |
| `get_agent` | Get full detail (status, workdir, subject, events, worktree) for one agent |
| `spawn_agent` | Spawn a new agent — pass a `prompt` for a quick auto-typed agent, or `type`+`repo` for a managed worktree; set `supervised: true` for `--permission-mode acceptEdits` instead of full bypass |
| `adopt_agent` | Register an existing Claude session: resume newest-for-dir under tmux, or live-register a running tmux session |
| `send_to_agent` | Type a message into a specific agent's claude session |
| `get_agent_output` | Return the recent terminal output of a specific agent |
| `terminate_agent` | Stop an agent (kill tmux + claude); keeps the record and worktree. Reversible via `restore_agent` — the default "stop this agent" action |
| `restore_agent` | Recreate and resume a lost/orphaned agent's session (`claude --resume`) |
| `delete_agent` | Clear an agent's stored record (archives by default; `hard` purges). Does not touch tmux or the worktree |
| `remove_worktree` | Remove an agent's git worktree + branch — **destructive**; refuses while the agent runs or has uncommitted/unpushed work unless `force` |

Example orchestrator prompts:

- "What is PROJ-350 doing?" — calls `get_agent` to fetch current status and events
- "Tell PROJ-343 to run the tests" — calls `send_to_agent` with `"run the tests"`
- "List all my agents" — calls `list_agents`
- "Spin up an agent to research SSE reconnection" — calls `spawn_agent` with a `prompt` (auto-typed)
- "Spawn a buildkite-debug agent in /path/to/repo" — calls `spawn_agent` with `type`+`repo`
- "Stop PROJ-350" — calls `terminate_agent` (reversible); "clear its record too" — then `delete_agent`

### Drive it from Claude (the `agentctl` skill)

Beyond raw tool access, install the packaged **Claude Code skill** so any Claude
session knows *how and when* to manage your fleet (triage, create-from-prompt,
relay "tell X to do Y", terminate-with-confirmation, daemon-down handling):

```sh
make install-skill   # symlinks skills/agentctl into ~/.claude/skills/agentctl
```

With the MCP server registered (above) and the skill installed, just talk to a
Claude session: *"list my agents"*, *"spin up an agent to research X"*,
*"what is agent-4f2a doing?"*, *"tell agent-4f2a to run the tests"*, *"kill the
idle ones"* — it drives the MCP tools (falling back to the `agentctl` CLI if the
MCP server isn't registered). The daemon must be running.

---

## Typical workflow

```sh
# 1. Start the daemon (once — then managed by launchd)
./scripts/install.sh   # install + start as a background launchd service (recommended)
make run-daemon        # foreground, for debugging only (blocks the terminal; ctrl-C to stop)

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
make run-daemon     # build + start daemon in the foreground (debugging only)
```

All tests run without Docker or any external services:

```bash
go test ./...
```

---

## Web GUI

The daemon embeds a React dashboard (Astro + React) and serves it at `http://localhost:8765` alongside the REST API — no separate server required.

### Build & run

```sh
make release     # 1. builds the Astro UI (web/), 2. embeds it via go:embed, 3. builds bin/agentctl
agentctl daemon  # start the daemon as usual
```

Then open `http://localhost:8765` in a browser.

> **Note:** the UI is baked into the binary at build time. After changing anything under `web/`, re-run `make release` (or `make ui` to rebuild only the frontend) and restart the daemon for the changes to take effect.

### What it does

The dashboard is a **tabbed mission-control shell**: two fixed tabs — **Overview** and **Cockpit** — plus one closeable tab per agent you pin.

- **Overview tab** — live fleet list over SSE (no manual refresh), each row with a coloured busy/idle badge (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) and the agent's auto-generated **subject**, plus fleet stats and an **attention queue** that surfaces agents in `waiting_for_input`/`errored`/`orphaned`.
- **Cockpit tab** — a multi-pane view for watching several agents at once.
- **Agent tabs** — pin any agent to its own tab to get a **live, interactive terminal** (`AttachTerminal`) — a real `tmux attach` bridged to the browser over a WebSocket, so you can type into the agent and watch it respond in real time. (The old read-only polled snapshot + separate send box were removed.)
- **Create agent** — **+ New agent** opens a prompt box (with a directory picker and a **Supervised** checkbox). Type the task and press **Create** (or Cmd/Ctrl+Enter); the type label is assigned automatically. Tick **Supervised** to launch with `--permission-mode acceptEdits` instead of full bypass. For a managed worktree, use the CLI: `agentctl start TICKET --type development --repo …`.
- **Terminate** — surfaces the git guard (409 → **Force** + optional **hard-delete**) when there's uncommitted/unpushed work.
- **Browser notifications** — opt in to get a desktop notification when an agent enters `waiting_for_input` (gated so they only fire while the tab is hidden).

### Dev workflow

Run two terminals in parallel — no rebuild loop needed while iterating on the UI:

```sh
# Terminal 1 — daemon (REST API + SSE on :8765)
agentctl daemon

# Terminal 2 — Astro dev server (:4321, proxies /sessions (incl. the /attach WebSocket) /spawn /events /healthz to :8765)
make ui-dev
```

Open `http://localhost:4321`. Edits under `web/src/` trigger HMR instantly; the browser stays on the same origin as the real daemon API so SSE and all REST calls work without CORS configuration.

### Tests

```sh
make web-test    # Vitest — frontend unit tests (status mapping, API client)
go test ./...    # Go suite — covers daemon hub, SSE endpoint, static embed, and all existing routes
```

The frontend Vitest suite lives in `web/src/lib/` alongside the source files (`status.test.ts`, `api.test.ts`). The Go daemon tests cover the broadcaster (`hub_test.go`), the SSE handler (`sse_test.go`), and the static file serving with SPA fallback (`static_test.go`).
