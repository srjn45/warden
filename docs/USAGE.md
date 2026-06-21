# Using warden

A practical, task-oriented guide to running `warden` day to day. For build,
install, and contributor details see the [README](../README.md); for a complete
catalog of what warden can do see [FEATURES.md](FEATURES.md); this document
focuses on **how to use the tool once it's installed**.

> `alias agents=warden` is handy (and a built-in `wd` symlink aliases `warden`) — every command below works under either name.

---

## 1. What warden is (the mental model)

`warden` (aliased as `wd`) lets you run many **Claude Code agent sessions** in parallel and
watch them from one place. Each agent is a real `claude` process running inside
its own detached **tmux** window. You spawn agents, watch what they're doing,
talk to them, and tear them down — without juggling terminals by hand.

One binary wears three hats:

| Face | What it is | You run it… |
|---|---|---|
| **daemon** | The single long-running process. Owns the on-disk session store, serves a loopback REST API on `127.0.0.1:8765`, and runs a background poller that keeps each agent's status and subject fresh. | Once, in the background (usually via launchd). |
| **CLI client** | `ls`, `status`, `start`, `done`, `attach`, `send`, `tail`, `tui` — thin HTTP clients that talk to the daemon. | Whenever you want to act on agents. |
| **MCP server** | `warden mcp` — a stdio bridge so an *orchestrator* Claude session can manage agents through tool calls. | Wired into a Claude session's MCP config. |

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
plist from `deploy/com.srajanpathak.warden.plist.template`, loads it, links the
Claude skill, and registers the MCP server. See the
[README](../README.md#install-the-daemon-as-a-launchd-service-auto-start) for
details (code-signing, redeploy, uninstall).

**Manual (for debugging — runs in the foreground):**

```sh
warden daemon      # or: warden daemon --addr 127.0.0.1:9000
```

Verify and inspect logs:

```sh
curl -s localhost:8765/healthz   # {"status":"ok"}
tail -f /tmp/warden.daemon.log # stdout  (launchd)
tail -f /tmp/warden.daemon.err # stderr  (launchd)
```

Stop / restart the launchd service:

```sh
launchctl unload ~/Library/LaunchAgents/com.srajanpathak.warden.plist   # stop
./scripts/reinstall.sh   # rebuild + redeploy + restart (or: make reinstall)
```

---

## 4. Quickstart — your first agent

The fastest path is **prompt mode**: give a plain-English task and let
warden handle the rest. No repo, no flags.

```sh
warden start "review the auth module for security issues"
# spawned agent-a1b2 (classifying…) — attach with `warden attach agent-a1b2`
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
warden ls                         # see it in the list
warden status agent-a1b2          # full detail + event history
warden tail agent-a1b2            # recent terminal output
warden send agent-a1b2 "also check the session cookie handling"
warden attach agent-a1b2          # drop into its terminal (Ctrl-b d to detach)
warden done agent-a1b2            # tear it down when finished
```

That's the whole loop. Everything else is variations on it.

---

## 5. Two ways to spawn

### Prompt mode (default — no worktree, auto-typed)

Just pass a quoted prompt. The agent launches in your current directory (or the
`--dir` you pass) and **assumes no worktree** — it operates directly on whatever
is in that directory. Use `--dir` to point it elsewhere.

```sh
warden start "investigate why the nightly build is flaky"
warden start "summarize the changes in /path/to/repo since last Friday"
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
warden start PROJ-350 --type development

# PR review (fresh worktree, runs `gh pr checkout`):
warden start --type pr-review --pr 1234

# Spike/analysis with an opt-in scratch worktree:
warden start --type spike --worktree

# Debug CI — no worktree, runs in the current repo:
warden start --type debug-ci

# Be explicit about repo and branch:
warden start PROJ-350 --type development --repo /path/to/repo --branch my-branch
```

**Type → worktree behavior:**

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | Creates `.worktrees/<ticket>` on a branch named after the ticket |
| `pr-review` | yes (PR branch) | Runs `gh pr checkout <PR>` inside it. **Requires `--pr` or `--branch`** |
| `analysis` | opt-in `--worktree` | Runs in the repo by default |
| `spike` | opt-in `--worktree` | Same as analysis |
| `code` | no | Runs in the repo root |
| `docs` | no | Runs in the repo root |
| `website` | no | Runs in the repo root |
| `debug-ci` | no | Runs in the repo root |
| `tests` | no | Runs in the repo root |
| `other` | no | Catch-all; also where unrecognized type strings land |

Notes:
- If a worktree for the ticket already exists on disk, the spawn **adopts** it
  rather than erroring.
- Every agent runs `claude --dangerously-skip-permissions` — permission
  prompts are suppressed, but the `Notification` hook still records them as
  events you can see in `status`.

---

## 5.1. Model Selection

Warden supports per-agent model selection via the `--model` flag. You can specify either a short alias or a full model ID. The model is stored with the session and preserved across restores.

### How to specify a model

**Prompt mode:**
```sh
warden start "Complex research task" --model opus
warden start "Quick analysis" --model haiku
```

**Managed worktree mode:**
```sh
warden start PROJ-350 --type development --model opus
warden start --type pr-review --pr 1234 --model sonnet
```

### Model aliases

Warden recognizes these short aliases:

| Alias | Full Model ID | Use Case |
|---|---|---|
| `opus` | `claude-opus-4-8` | Complex reasoning, large codebases, architectural decisions |
| `sonnet` | `claude-sonnet-4-6` | Balanced performance (default) |
| `haiku` | `claude-3-5-haiku-20241022` | Fast, lightweight tasks |
| `fable` | `claude-3-7-fable` | Experimental tasks |

You can also use any full model ID directly:
```sh
warden start "Task" --model claude-sonnet-4-6
```

### Default model

If you don't specify `--model`, warden uses:

1. **`WARDEN_MODEL_DEFAULT` environment variable** (if set)
2. **`claude-sonnet-4-6`** (built-in fallback)

Set your default model:
```sh
# In your shell profile (.bashrc, .zshrc, etc.)
export WARDEN_MODEL_DEFAULT=opus

# Or per-session
WARDEN_MODEL_DEFAULT=haiku warden start "Quick task"
```

The environment variable accepts either aliases or full model IDs:
```sh
export WARDEN_MODEL_DEFAULT=opus
export WARDEN_MODEL_DEFAULT=claude-opus-4-8   # same effect
```

### Viewing the model

The model is shown in multiple places:

**`warden ls`** — MODEL column:
```sh
warden ls
# ID          TYPE      STATUS   AGE  MODEL              DIR         SUBJECT
# PROJ-350    dev       working  5m   claude-opus-4-8    PROJ-350    refactoring auth
# agent-a1b2  analysis  idle     2m   claude-haiku-...   agent-a1b2  reviewing logs
```

**`warden status <id>`** — model field:
```sh
warden status PROJ-350
# id:       PROJ-350
# type:     development
# status:   working
# model:    claude-opus-4-8
# ...
```

### MCP tool usage

When spawning agents via the MCP `spawn_agent` tool (from an orchestrator Claude session), pass the `model` parameter:

```typescript
// Orchestrator Claude calls spawn_agent
spawn_agent({
  prompt: "Analyze the auth module",
  model: "opus"  // or "claude-opus-4-8"
})
```

The same default resolution applies: `WARDEN_MODEL_DEFAULT` → `claude-sonnet-4-6`.

### Restored agents

When you `warden restore <id>` an orphaned agent, it resumes with the **original model** it was spawned with — the model is stored in the session record and preserved.

---

## 6. Command reference

All commands accept `--addr` to point at a non-default daemon (overrides
`WARDEN_ADDR`). `<TICKET>` is the agent ID — a Jira key for managed agents,
or an `agent-xxxx` ID for prompt-spawned ones.

### `warden` / `warden tui`
Open the tmux-composited cockpit (see §7). Bare `warden` with no
subcommand does the same thing. Requires tmux (see §7).

### `warden start [TICKET|"<prompt>"] [flags]`
Spawn an agent. Prompt mode if no `--type`; managed-worktree mode otherwise.

| Flag | Meaning |
|---|---|
| `--type` | `development\|analysis\|spike\|pr-review\|code\|docs\|website\|debug-ci\|tests\|other`. Omit for prompt mode. |
| `--repo` | Repo path (default: current directory; managed mode only). |
| `--branch` | New branch (development) or checkout target (pr-review). |
| `--pr` | PR number/URL (pr-review). |
| `--worktree` | Create a scratch worktree for analysis/spike. |
| `--model` | Model to use: short alias (`opus`/`sonnet`/`haiku`/`fable`) or full model ID. Default: `WARDEN_MODEL_DEFAULT` env var, or `claude-sonnet-4-6`. |

### `warden ls`
List all active sessions: `ID  TYPE  STATUS  AGE  DIR  SUBJECT`.
`DIR` is the base name of the working directory; `SUBJECT` is empty until the
first poller refresh; `TYPE` shows `…` while a prompt agent is still being
classified.

### `warden status <TICKET>`
Full detail for one session: id, type, ticket, status, repo, workdir,
worktree, branch, pr, subject, last-updated, and the full event timeline.

### `warden tail <TICKET> [--lines N]`
Print the recent terminal output of the agent's claude session
(default 200 lines).

```sh
warden tail PROJ-350 --lines 80
```

### `warden send <TICKET> <message...>`
Type a message into the agent's claude session and press Enter — exactly as if
you'd typed it at the prompt.

```sh
warden send PROJ-350 "run the unit tests and fix any failures"
```

### `warden adopt [--session-id <uuid>] [--dir <path>]`
Register an existing Claude session into warden.

- **Plain shell** — finds the newest Claude conversation for the directory and
  resumes it under a new tmux session (`claude --resume`).
- **Inside tmux** — registers the current tmux session live without relaunching
  claude.

```sh
warden adopt                          # newest session for cwd, resume under tmux
warden adopt --session-id <uuid>      # pick a specific Claude conversation
warden adopt --dir /path/to/project   # target a different directory
```

### `warden attach <TICKET>`
Hand your terminal to the agent's tmux session interactively. Detach with the
tmux prefix-then-`d` (default `Ctrl-b d`) to leave the agent running.

### `warden done <TICKET> [--hard]`
Terminate the agent (kill its tmux + claude session) **and** clear its record in
one step — equivalent to `terminate` then `delete`. It does **not** remove the
git worktree; that's a separate, explicitly-confirmed step (`remove-worktree`).

```sh
warden done PROJ-350          # terminate + clear record (worktree kept)
warden done PROJ-350 --hard   # purge the record instead of archiving it
```

### `warden terminate <TICKET>`
Stop an agent — kill tmux + claude — but **keep** the record and worktree. The
safe "stop this agent" default; reversible with `warden restore`.

### `warden restore <TICKET>`
Recreate and resume a lost/orphaned agent (`claude --resume`). Use only when the
agent's tmux session is gone (status `orphaned`).

### `warden delete <TICKET> [--hard]`
Clear an agent's stored record (archives by default; `--hard` purges). Leaves
tmux and the worktree alone.

### `warden remove-worktree <TICKET> [--force]`
Remove an agent's git worktree and branch. **Destructive** and **guarded** — it
refuses while the agent is still running (terminate it first) or while the
worktree has uncommitted changes or unpushed commits. `--force` overrides the
guard.

```sh
warden remove-worktree PROJ-350
warden remove-worktree PROJ-350 --force
```

### `warden daemon [--addr ADDR]`
Run the hub (HTTP API + poller). Normally launchd's job; run by hand to debug.

### `warden mcp [--addr ADDR]`
Run the MCP stdio server (see §8).

### `warden digest <TICKET> [--json]`
Summarize what an agent accomplished — files touched, branch, turn count, and a
short narrative. Also a web **Digest** panel and the cockpit `d` key.

### `warden approvals` / `warden approve <TICKET> <option>`
With the approvals inbox on (`WARDEN_APPROVALS`, default on), `approvals` lists
recognized tool-permission prompts waiting for an answer (each with its numbered
options), and `approve` answers one by option number — without attaching. Also
answerable from the web AttentionQueue or the cockpit (the **⏳ Approvals** row →
`i`/`enter`, then `1`-`9`; `tab` cycles waiting agents). Unrecognized prompts fall
back to attach.

```sh
warden approvals
warden approve PROJ-350 1     # answer with option 1 (e.g. "Yes")
```

### `warden rotate --confirm --resume-file <path> --resume-prompt <text>`
Run **inside an agent session** to retire a context-heavy agent and hand off to
a fresh successor in the same workdir/worktree. Phase 1 is driven by the
`/warden` skill (writes the handoff + resume prompt); `--confirm` then spawns the
successor and reaps the current agent (spawn-before-reap, fail-safe; never
removes the worktree).

### `warden ctx set|get|list|del`
Read/write the **shared context** — a namespaced key/value blackboard all agents
can use to collaborate. Writer defaults to `$WARDEN_SESSION_ID` (else `human`);
override with `--as`.

```sh
warden ctx set build.status green --as agent-4f2a
warden ctx get build.status
warden ctx list --prefix build.
warden ctx del build.status
```

### `warden msg send|inbox|wait`
**Directed messages** between agents. Sending wakes a parked (idle/waiting)
recipient; `wait` blocks in the daemon (long-poll) until a message arrives.

```sh
warden msg send agent-9c1d "the API contract changed — re-check your client"
warden msg inbox --as agent-9c1d
warden msg wait --as agent-9c1d --timeout 120
```

### `warden pipeline create|start|show|list|cancel|retry|edit-job|delete`
Define and run a **DAG of agent jobs** from a YAML spec (CLI-only authoring). See
§7.5 below for the full guide.

### `warden doctor`
Preflight checks — required binaries (`tmux`, `git`, `claude`, `gh`), daemon
reachability, and the data directory.

---

## 7. The terminal UI (TUI)

```sh
warden tui     # or just: warden
```

### The tmux-composited cockpit (default)

`warden tui` builds a **tmux-composited cockpit** — a dedicated tmux session
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
| `←`/`→` or `h`/`l` | Collapse / expand the pipeline under the cursor |
| `Enter` | Open the selected agent (or running pipeline job) in the right detail pane |
| `n` | New agent — opens a prompt textarea; `ctrl+s` to submit, `esc` to cancel |
| `o` | Open a directory as a group (becomes the spawn target for `n`) |
| `s` | Send a message to the selected agent — `enter` to send, `esc` to cancel |
| `a` | Attach — hands the whole client to the agent's (or running job's) tmux session. Press **`Ctrl-b Enter`** to return to the dashboard (a hint flashes on attach). |
| `d` | Completion digest for the selected agent — scrollable overlay (`d`/`esc` to close) |
| `i` | Answer pending approvals (also `enter` on the **⏳ Approvals** row) — `1`-`9` to answer, `tab` for next |
| `c` | Shared-context + message-traffic inspector |
| `r` | Retry a failed / needs-attention pipeline job |
| `x` | Context-sensitive — terminate the selected agent / cancel a pipeline / close an opened dir (confirm with `y`) |
| `D` | Delete a stopped pipeline's record (confirm with `y`) |
| `?` | Toggle help |
| `q` | Quit and tear down the whole cockpit |

Pipelines appear in the list pane under a **▸ Pipelines** section (one header row
per pipeline, then an indented row per job with a status glyph). Collapse/expand a
pipeline with `←`/`→` (or `h`/`l`). On a pipeline row, `x` cancels it and `D`
deletes a stopped pipeline's record; on a job row, `r` retries a
failed/needs-attention job, and `enter`/`a` opens a running job's session.
(Authoring pipelines is via `warden pipeline create -f` — see §7.5; editing job
prompts and building pipelines in the TUI are not yet available.)

> **Getting back from an agent.** Attaching moves your single tmux client onto
> the agent's session (tmux can't nest an attach), so use **`Ctrl-b Enter`** to
> jump back to the dashboard — not `Ctrl-b d`. `Ctrl-b d` still works but it
> *detaches* the cockpit to the background rather than returning to it; the
> cockpit survives (it's reaped on your next `warden tui`), so an accidental
> detach no longer destroys your dashboard. Only `q` tears it down.

**Bottom-left — terminal shell.** A live shell (`$SHELL`) running in the directory
where you launched the cockpit. Use this for running `warden` CLI commands, checking
git status, or any other terminal work while monitoring your agents. Unlike the old
embedded Claude pane, this gives you direct command-line access.

**Right (full height) — live agent detail pane.** When you press `Enter` on an
agent in the list, a live, interactive terminal of that agent's `claude` session
opens here. You can type directly into the agent, read its output, and watch it
respond in real time. Scrolling the agents list with `↑`/`↓` or `j`/`k` does not
replace this pane, so an agent you're actively working with is never interrupted
by casual browsing. Press `Enter` again on a different agent to switch.

To move focus between panes without leaving the cockpit, use **Alt+←/→/↑/↓**
(no tmux prefix needed).

> **Caveats — nested tmux and Alt+Arrow navigation:**
> Because the right pane runs a tmux client nested inside the cockpit session,
> the normal tmux prefix (`Ctrl-b`) is ambiguous there and will be captured by
> the outer session. Use **Alt+Arrow** to move between panes instead. These
> bindings are applied tmux-server-wide, so they will also affect any other tmux
> sessions you have open on the same server. Requires **tmux ≥ 3.1**.

Each cockpit launch creates an independent tmux session (named
`warden-tui-<pid>`), so opening two terminals and running `warden tui` in
each gives you two separate cockpits, each with its own shell.

### Requirements

The cockpit requires **tmux ≥ 3.1** — it composites real tmux panes. There is no
single-pane fallback: if tmux isn't installed, or you run `warden tui` from
**inside an existing tmux session** (which would nest sessions), the cockpit
can't build its panes and exits with an error. Run it from a plain terminal.

The list pane polls the daemon about once a second, so the daemon must be
running before you open the TUI.

---

## 7.5 Pipelines (DAG of agent jobs)

A **pipeline** is a DAG of agent jobs defined in YAML. The daemon runs it: jobs
with no dependencies start first, and each job's `emit` publishes its output and
unblocks its dependents — so a "lead" Claude stays off the critical path.
Authoring is CLI-only (`warden pipeline create -f`); the TUI and web show + control
pipelines but don't author them.

**Lifecycle:**

```sh
warden pipeline create -f review.yaml   # validate + register (does NOT start)
warden pipeline start <id>              # spawn jobs with no dependencies
warden pipeline show <id>               # jobs, status, branches, emitted output
warden pipeline list
warden pipeline retry <id> <job>        # re-run a failed/needs-attention job
warden pipeline edit-job <id> <job> --prompt "…"   # edit a still-pending job
warden pipeline cancel <id>             # terminate running jobs
warden pipeline delete <id>             # remove the record (cancel first if live)
```

**Spec** — a minimal `analyze → implement → review` chain. **Important:** job
prompts must **not** mention `emit` — the daemon auto-appends the emit step and
auto-injects each upstream job's output into the dependents' prompts.

```yaml
name: auth-refactor
jobs:
  - id: analyze
    prompt: "Analyze the auth module and list the concrete refactors needed."
  - id: implement
    depends_on: [analyze]
    worktree: fresh          # none | fresh | from:<base-branch>
    prompt: "Implement the refactors identified upstream."
  - id: review
    depends_on: [implement]
    prompt: "Review the implementation branch for correctness and regressions."
```

Results are durable in the pipeline record (`warden pipeline show`), the shared
context (`pipeline.<id>.<job>.output`), and each job's git branch — they are not
tied to the (possibly reaped) live agent.

---

## 8. Orchestrating agents from another Claude session (MCP)

Register `warden mcp` as an MCP server in your *orchestrator* Claude session
so it can manage agents via tool calls. Add to your MCP config (e.g.
`~/.claude/claude_desktop_config.json` or a project `.claude/mcp.json`):

```json
{
  "mcpServers": {
    "warden": {
      "command": "warden",
      "args": ["mcp"],
      "env": { "WARDEN_ADDR": "127.0.0.1:8765" }
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
- *"Spawn a debug-ci agent in /path/to/repo"* → `spawn_agent`
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
warden daemon
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

The web dashboard also has a **Pipelines** tab: it lists pipelines, shows a
selected pipeline's jobs as status-colored cards with dependency chips, and a
per-job drawer with the prompt/handoff/output, a **Cancel** (pipeline) /
**Retry** (job) control, and an **Open terminal** link to a running job's
session. (Creating / editing pipelines in the browser is not yet available —
use `warden pipeline create -f`.)

> The UI is baked into the binary at build time. After changing anything under
> `web/`, rebuild (`make release`, or `make ui` for the frontend only) and
> restart the daemon. For live UI iteration, run `warden daemon` and
> `make ui-dev` in parallel and open `http://localhost:4321`.

---

## 11. Configuration

Set via environment variables (or override the daemon address per-command with
`--addr`):

| Variable | Default | Description |
|---|---|---|
| `WARDEN_ADDR` | `127.0.0.1:8765` | Daemon listen/connect address |
| `WARDEN_DATA_DIR` | `~/.warden` | Directory for session JSON files (`sessions/`, `closed/`) and prompt files (`prompts/`) |
| `WARDEN_WORKDIR` | `~/warden-agents` | Where the per-agent prompt file is stored — **not** where the agent runs (prompt agents run in the caller's cwd or `--dir`) |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects |
| `WARDEN_MODEL_DEFAULT` | `claude-sonnet-4-6` | Default model for new agents; can be a short alias (`opus`/`sonnet`/`haiku`/`fable`) or full model ID. Overridden by `--model` flag |
| `WARDEN_NOTIFY` | `off` | macOS desktop notifications when an agent needs attention (`on`/`1`/`true` to enable) |
| `WARDEN_TOKEN_GUARD` | `on` | Context-size guard master switch: read each live agent's context-window fill from its transcript, classify `ok`/`warning`/`critical`, and show a state-colored token figure in `ls`/TUI/web. Disable with `0`/`off`/`false` |
| `WARDEN_TOKEN_WARN_ALERT` | `on` | Fire a desktop notification (when `WARDEN_NOTIFY` is on) once per upward crossing into warning/critical. Disable with `0`/`off`/`false` |
| `WARDEN_TOKEN_AUTO_COMPACT` | `on` | Auto-send `/compact` when an agent is `critical` and idle/waiting (cooldown-guarded). Disable with `0`/`off`/`false` |
| `WARDEN_TOKEN_WARN` | `200000` | Warning threshold in context tokens (inclusive). Both thresholds reset to defaults if critical ≤ warn |
| `WARDEN_TOKEN_CRITICAL` | `400000` | Critical threshold in context tokens (inclusive) — the auto-`/compact` trigger band |

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
| `rate_limited` | Hit API session limit — auto-resuming when limit expires (§12.1) |

---

## 12.1. Rate limit handling

Warden **automatically detects and handles** Claude API rate limits:

### Detection

When an agent hits the API session limit, warden:
1. Detects the rate limit from the agent's terminal output
2. Transitions status to `rate_limited` (shown in **yellow/amber**)
3. Schedules automatic resume when the limit expires

### Viewing rate-limited agents

```bash
# List all agents (rate_limited shown in yellow)
warden ls

# View detailed rate limit info with countdown
warden status <agent-id>
```

Example output:
```
status:     rate_limited
rate limit:
  limited at: 2026-06-15 14:30:00
  resume at:  2026-06-15 15:45:00 (in 1h 15m 23s)
  retries:    0
```

### Auto-resume behavior

- **Parsed timestamp**: Resumes at the exact time + 1 minute safety buffer
- **No timestamp found**: Retries every 30 minutes until successful
- **Still limited on retry**: Re-parses error for updated time, reschedules
- **Non-limit error**: Transitions to `errored` status

### Configuration

Environment variables for the daemon:

```bash
# Disable auto-resume (manual intervention only)
WARDEN_RATE_LIMIT_AUTO_RESUME=false warden daemon

# Change retry interval (default: 30m)
WARDEN_RATE_LIMIT_RETRY_INTERVAL=15m warden daemon

# Change safety buffer after parsed time (default: 1m)
WARDEN_RATE_LIMIT_BUFFER=2m warden daemon
```

### Manual intervention

You can override the scheduler:

```bash
# Manually resume immediately (bypasses timer)
warden attach <agent-id>

# Terminate if no longer needed
warden done <agent-id>
```

The scheduler persists across daemon restarts — if you restart the daemon,
rate-limited agents will have their timers reconstructed and will resume at the
scheduled time.

---

## 13. Typical workflows

**Ad-hoc investigation (prompt mode):**
```sh
warden start "find and fix the flaky test in the payments suite"
warden ls
warden tail <id>
warden send <id> "skip the integration tests for now"
warden done <id>
```

**Ticketed development (managed worktree):**
```sh
warden start PROJ-350 --type development     # worktree + branch
warden status PROJ-350
warden attach PROJ-350                       # jump in when needed
warden done PROJ-350                          # guarded teardown
```

**Reviewing a PR:**
```sh
warden start --type pr-review --pr 1234
warden tail prreview-... 
warden done prreview-...
```

---

## 14. Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| Any command hangs or errors connecting | Daemon not running. `curl localhost:8765/healthz`; start it (§3). |
| `healthz` fails / daemon won't start | Data dir not writable. Check `WARDEN_DATA_DIR` (default `~/.warden`) and `/tmp/warden.daemon.err`. |
| New agent stuck at `classifying…` / type is `other` | `claude` not on the daemon's PATH. Type falls back to `other`; functionality is otherwise fine. |
| `SUBJECT` stays empty | Poller hasn't refreshed yet (it's throttled and only runs when pane content changes), or `CLAUDE_PROJECTS_DIR` is wrong. |
| `pr-review needs --pr or --branch` | pr-review requires one of those flags. |
| `remove-worktree` refuses | The agent is still running (terminate it first) or the worktree has uncommitted/unpushed work — the guard is protecting it. Commit/push, or use `--force`. (`done` no longer touches the worktree.) |
| Status never updates live | Hooks not wired into `~/.claude/settings.json` (§9). The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents launch in your current directory — `cd` to the right place first, or pass `--dir <path>`. |
| Agent stuck in `rate_limited` | Auto-resume disabled or timer not firing. Check daemon logs, or manually `attach` to override. See §12.1 for configuration. |

---

## 15. Shared context

A namespaced key/value store the daemon owns, so agents can share results.

```sh
warden ctx set global.findings "auth.py needs refactor"   # inline value
warden ctx set report.body --file ./report.md             # value from a file
some-command | warden ctx set logs.tail --stdin           # value from stdin
warden ctx get global.findings                            # prints the value
warden ctx list pipeline.                                 # keys under a prefix
warden ctx del global.findings
```

Writes are attributed to `$WARDEN_SESSION_ID` when set (so a spawned agent's
writes are tagged with its id), otherwise to `human`. Override with `--as`.
Keys are free-form dot-namespaced strings (`global.*`, `pipeline.<id>.*`,
`agent.<sid>.*`). Also available as MCP tools `ctx_set` / `ctx_get` / `ctx_list`.

---

## 16. Directed messages

Agent-to-agent messages with a durable per-recipient inbox.

```sh
warden msg send <agent-id> "can you check the auth module?"   # deliver + wake if idle
warden msg inbox                                              # read my messages (marks read)
warden msg inbox --unread                                     # only unread
warden msg wait --from <agent-id> --timeout 120               # block until a reply (one call)
```

Sending **wakes the recipient only if it's idle or waiting** — a working agent is
never interrupted; its message waits in the inbox. `msg wait` blocks in the
daemon (a long-poll), so an agent awaits a reply in a single call with no
busy-loop. Identity defaults to `$WARDEN_SESSION_ID`, which warden sets on every agent's
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
warden pipeline create -f refactor.yaml   # validate the DAG (cycles, unknown refs)
warden pipeline start refactor-auth        # spawn all jobs with no deps immediately
warden pipeline show refactor-auth         # DAG + per-job status
warden pipeline cancel refactor-auth       # terminate running jobs + mark canceled
```

Each job's agent finishes by running `warden pipeline emit "<handoff>"`. The
pipeline and job IDs are injected into every job's environment automatically
(`WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`), so the agent just runs the command
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
warden pipeline edit-job <pipeline> <job> --prompt "..." --handoff "..."
warden pipeline retry <pipeline> <job>
```

`edit-job` tweaks a job's prompt and/or handoff *before it starts* (pending jobs
only). If a job's agent goes quiet without emitting (its session is flagged
`idle` by stuck-detection), the job is marked **`needs_attention`** rather than
silently stalling — the pipeline stays `running` and the job is shown flagged.
Resolve it by `pipeline emit`-ing on the job's behalf (if the agent actually
finished) or `pipeline retry`, which tears down the stale job session/worktree,
resets the job, reopens any descendants that were skipped, and re-runs from there.
