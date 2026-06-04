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
./scripts/install.sh   # or: make install
```

The installer builds the release, installs the binary, renders the launchd
plist from `deploy/com.srajanpathak.agentctl.plist.template`, loads it, links the
Claude skill, and registers the MCP server. See the
[README](../README.md#install-the-daemon-as-a-launchd-service-auto-start) for
details (code-signing, redeploy, uninstall).

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

Stop / restart the launchd service:

```sh
launchctl unload ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist   # stop
./scripts/reinstall.sh   # rebuild + redeploy + restart (or: make reinstall)
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

### `agentctl done <TICKET> [--hard]`
Terminate the agent (kill its tmux + claude session) **and** clear its record in
one step — equivalent to `terminate` then `delete`. It does **not** remove the
git worktree; that's a separate, explicitly-confirmed step (`remove-worktree`).

```sh
agentctl done PROJ-350          # terminate + clear record (worktree kept)
agentctl done PROJ-350 --hard   # purge the record instead of archiving it
```

### `agentctl terminate <TICKET>`
Stop an agent — kill tmux + claude — but **keep** the record and worktree. The
safe "stop this agent" default; reversible with `agentctl restore`.

### `agentctl restore <TICKET>`
Recreate and resume a lost/orphaned agent (`claude --resume`). Use only when the
agent's tmux session is gone (status `orphaned`).

### `agentctl delete <TICKET> [--hard]`
Clear an agent's stored record (archives by default; `--hard` purges). Leaves
tmux and the worktree alone.

### `agentctl remove-worktree <TICKET> [--force]`
Remove an agent's git worktree and branch. **Destructive** and **guarded** — it
refuses while the agent is still running (terminate it first) or while the
worktree has uncommitted changes or unpushed commits. `--force` overrides the
guard.

```sh
agentctl remove-worktree PROJ-350
agentctl remove-worktree PROJ-350 --force
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

Pipelines appear in the list pane under a **▸ Pipelines** section (one header row
per pipeline, then an indented row per job with a status glyph) — in both the
cockpit and `--classic` views. On a pipeline row, `x` cancels it; on a job row,
`r` retries a failed/needs-attention job, and `a` (or `enter` in the cockpit)
opens a running job's session. In the `--classic` single-pane view, selecting a
pipeline header also renders its full DAG (per-job status + captured outputs)
inline in the detail pane. (Authoring pipelines is via `agentctl pipeline create
-f`; editing job prompts and building pipelines in the TUI are not yet available.)

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
| `spawn_agent` | Spawn a new agent — `prompt` for a quick auto-typed one, or `type`+`repo` for a managed worktree |
| `adopt_agent` | Register an existing Claude session: resume newest-for-dir under tmux, or live-register a running tmux session |
| `send_to_agent` | Type a message into a specific agent's claude session |
| `get_agent_output` | Recent terminal output of a specific agent |
| `terminate_agent` | Stop an agent (kill tmux + claude); keeps record + worktree. Reversible via `restore_agent` — the default "stop" action |
| `restore_agent` | Recreate and resume a lost/orphaned agent (`claude --resume`) |
| `delete_agent` | Clear an agent's record (archives by default; `hard` purges) |
| `remove_worktree` | Remove an agent's worktree + branch — **destructive**; refuses while running or with unsaved work unless `force` |

Then just talk to the orchestrator naturally:

- *"What is PROJ-350 doing?"* → `get_agent`
- *"Tell PROJ-343 to run the tests"* → `send_to_agent`
- *"List all my agents"* → `list_agents`
- *"Spawn a buildkite-debug agent in /path/to/repo"* → `spawn_agent`
- *"Stop PROJ-350"* → `terminate_agent` (reversible); "clear its record too" → `delete_agent`

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

It's a **tabbed mission-control shell**: fixed **Overview** and **Cockpit** tabs
plus one closeable tab per pinned agent. Overview has the live SSE agent list,
busy/idle badges, fleet stats, an **attention queue** (agents in
`waiting_for_input`/`errored`/`orphaned`), and a **+ New agent** prompt box with
a directory picker. Pin an agent to its own tab to get a **live, interactive
terminal** — a real `tmux attach` bridged to the browser over a WebSocket, so
you can type into the agent and watch it respond (no more read-only snapshot).
The **Terminate** button surfaces the same git guard (with **Force** and an
optional hard-delete) when there's unsaved work. Opt in to **browser
notifications** to be pinged when an agent needs input while the tab is hidden.

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
| `AGENTCTL_WORKDIR` | `~/agentctl-agents` | Where the per-agent prompt file is stored — **not** where the agent runs (prompt agents run in the caller's cwd or `--dir`) |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects |
| `AGENTCTL_NOTIFY` | `off` | macOS desktop notifications when an agent needs attention (`on`/`1`/`true` to enable) |

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
| `remove-worktree` refuses | The agent is still running (terminate it first) or the worktree has uncommitted/unpushed work — the guard is protecting it. Commit/push, or use `--force`. (`done` no longer touches the worktree.) |
| Status never updates live | Hooks not wired into `~/.claude/settings.json` (§9). The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents launch in your current directory — `cd` to the right place first, or pass `--dir <path>`. |

---

## 15. Shared context

A namespaced key/value store the daemon owns, so agents can share results.

```sh
agentctl ctx set global.findings "auth.py needs refactor"   # inline value
agentctl ctx set report.body --file ./report.md             # value from a file
some-command | agentctl ctx set logs.tail --stdin           # value from stdin
agentctl ctx get global.findings                            # prints the value
agentctl ctx list pipeline.                                 # keys under a prefix
agentctl ctx del global.findings
```

Writes are attributed to `$AGENTCTL_SESSION_ID` when set (so a spawned agent's
writes are tagged with its id), otherwise to `human`. Override with `--as`.
Keys are free-form dot-namespaced strings (`global.*`, `pipeline.<id>.*`,
`agent.<sid>.*`). Also available as MCP tools `ctx_set` / `ctx_get` / `ctx_list`.

---

## 16. Directed messages

Agent-to-agent messages with a durable per-recipient inbox.

```sh
agentctl msg send <agent-id> "can you check the auth module?"   # deliver + wake if idle
agentctl msg inbox                                              # read my messages (marks read)
agentctl msg inbox --unread                                     # only unread
agentctl msg wait --from <agent-id> --timeout 120               # block until a reply (one call)
```

Sending **wakes the recipient only if it's idle or waiting** — a working agent is
never interrupted; its message waits in the inbox. `msg wait` blocks in the
daemon (a long-poll), so an agent awaits a reply in a single call with no
busy-loop. Identity defaults to `$AGENTCTL_SESSION_ID`, which agentctl sets on every agent's
tmux session automatically — so inside an agent, `msg` and `ctx` commands just
work without flags. Pass `--as <agent-id>` only to act as a different agent (e.g.
a human operator or a lead agent answering on another's behalf). Also available as MCP tools
`send_message` / `read_inbox` (no MCP `wait` — use the CLI for blocking waits).

Request/reply pattern: A runs `msg send B "..."` then `msg wait --from B`; B reads
its inbox, does the work, and replies with `msg send A "..."`, unblocking A.

---

## 17. Pipelines

Run a **DAG of agent jobs**. Each job spawns as a normal agent when its
dependencies finish; outputs (and branch names) flow downstream automatically.

Author a spec (`refactor.yaml`):

```yaml
name: refactor-auth
repo: /Users/me/workspace/app
jobs:
  - id: analyze
    prompt: "Analyze the auth module; no code yet."
    worktree: none
  - id: implement
    prompt: "Implement the refactor described upstream."
    depends_on: [analyze]
    worktree: fresh
    handoff: "the branch name and a 2-line summary"
  - id: review
    prompt: "Merge the implement branch, review, run the suite."
    depends_on: [implement]
    worktree: from:implement
```

Then:

```sh
agentctl pipeline create -f refactor.yaml   # validate the DAG (cycles, unknown refs)
agentctl pipeline start refactor-auth        # spawn all jobs with no deps immediately
agentctl pipeline show refactor-auth         # DAG + per-job status
agentctl pipeline cancel refactor-auth       # terminate running jobs + mark canceled
```

Each job's agent finishes by running `agentctl pipeline emit "<handoff>"`. The
pipeline and job IDs are injected into every job's environment automatically
(`AGENTCTL_PIPELINE_ID`, `AGENTCTL_JOB_ID`), so the agent just runs the command
with no flags. Emitting publishes the handoff text to shared context, marks the
job `done`, and unblocks any dependents.

**Worktree strategies** (`worktree:` field):

| Value | Behaviour |
|---|---|
| `none` | Agent runs in the repo root; no git worktree created |
| `fresh` | A new git worktree is created on a branch named `<pipeline>-<job>` off HEAD |
| `from:<job>` | A new git worktree is created off the upstream job's branch (for fan-in merges) |

`worktree: from:<job>` bases a job's git worktree on the upstream job's branch.
A fan-in job (e.g. `review` above) does the `git merge` itself as part of its
prompt work.

**Failure behaviour:** if a job's agent session enters `errored` or `orphaned`,
the job is marked `failed`, its descendants are marked `skipped`, and the
pipeline status becomes `stalled`. Jobs that were already running are not
interrupted — only pending descendants are skipped. A `stalled` pipeline can be
inspected with `pipeline show` and cleaned up with `pipeline cancel`.

**Pipeline status values:**

| Status | Meaning |
|---|---|
| `pending` | Created, not yet started |
| `running` | At least one job is in progress |
| `done` | All jobs finished successfully |
| `stalled` | A job failed; its descendants have been skipped |
| `canceled` | Explicitly canceled by the user |

**Editing and recovery:**

```sh
agentctl pipeline edit-job <pipeline> <job> --prompt "..." --handoff "..."
agentctl pipeline retry <pipeline> <job>
```

`edit-job` tweaks a job's prompt and/or handoff *before it starts* (pending jobs
only). If a job's agent goes quiet without emitting (its session is flagged
`idle` by stuck-detection), the job is marked **`needs_attention`** rather than
silently stalling — the pipeline stays `running` and the job is shown flagged.
Resolve it by `pipeline emit`-ing on the job's behalf (if the agent actually
finished) or `pipeline retry`, which tears down the stale job session/worktree,
resets the job, reopens any descendants that were skipped, and re-runs from there.
