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
warden ls --watch                 # live table, redraws on every state change (Ctrl+C to exit)
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

# Debug CI — isolated in its own worktree by default:
warden start --type debug-ci

# Opt a write-agent out of isolation to share the repo:
warden start --type debug-ci --in-repo

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
| `code` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `docs` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `website` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `debug-ci` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `tests` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `other` | no | Catch-all; also where unrecognized type strings land |

Every write-agent (development, pr-review, code, docs, website, debug-ci, tests)
is isolated in its own git worktree by default so parallel agents never collide
in the shared repo (Phase 0a). Pass `--in-repo` to deliberately run a write-agent
in the shared repo; `pr-review` ignores `--in-repo` because it is structurally a
separate checkout.

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
| `haiku` | `claude-haiku-4-5` | Fast, lightweight tasks |
| `fable` | `claude-fable-5` | Experimental tasks |

You can also use any full model ID directly:
```sh
warden start "Task" --model claude-sonnet-4-6
```

### Default model

If you don't specify `--model`, warden uses:

1. The **`model_default`** setting in your config file (`~/.warden/config.yaml`), if set
2. **`claude-sonnet-4-6`** (built-in fallback)

Set your default model by editing the config file (run `warden config init`
first if it doesn't exist yet), then restart the daemon:
```yaml
# ~/.warden/config.yaml
model_default: opus
```

`model_default` accepts either an alias or a full model ID:
```yaml
model_default: opus
model_default: claude-opus-4-8   # same effect
```

Run `warden config` to print the resolved configuration that is currently live.

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

The same default resolution applies: the `model_default` config setting → `claude-sonnet-4-6`.

### Restored agents

When you `warden restore <id>` an orphaned agent, it resumes with the **original model** it was spawned with — the model is stored in the session record and preserved.

---

## 6. Command reference

All commands accept `--addr` to point at a non-default daemon (overrides
the `addr` config setting). `<TICKET>` is the agent ID — a Jira key for managed agents,
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
| `--in-repo` | Write-agent opt-out: run in the shared repo instead of an isolated worktree (ignored for pr-review). |
| `--model` | Model to use: short alias (`opus`/`sonnet`/`haiku`/`fable`) or full model ID. Default: the `model_default` config setting, or `claude-sonnet-4-6`. |

### `warden ls`
List all active sessions: `ID  TYPE  STATUS  AGE  DIR  SUBJECT`.
`DIR` is the base name of the working directory; `SUBJECT` is empty until the
first poller refresh; `TYPE` shows `…` while a prompt agent is still being
classified.

### `warden search <QUERY...>`
Full-text search across your agents. Every whitespace-separated word is ANDed
against each session's id, name, ticket, type, subject, prompt, branch, and
last-pane excerpt (case-insensitive). Renders the same table as `warden ls`.

```sh
warden search auth refactor        # agents matching BOTH "auth" and "refactor"
warden search PROJ-350 --closed    # include archived sessions in the search
warden search payments --json      # raw records for scripting
```

### `warden history [--since <when>] [--type <type>] [--limit N]`
Browse agents that have ended (the persisted `closed/` archive, newest-first).
`--since` accepts a duration (`24h`, `90m`, `7d`, `2w`), a date (`2026-06-01`),
or an RFC3339 timestamp; `--type` filters by task type; `--limit` caps the count;
`--json` prints raw records.

```sh
warden history --since 7d                 # ended in the last week
warden history --type pr-review --limit 20
```

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

### `warden commit -m <msg>` / `warden push` / `warden sync` (the git lifecycle)
Run from inside an agent's worktree (also exposed as the MCP tools
`mcp__warden__commit` / `push` / `sync`). Each replaces a flurry of raw `git`
Bash with one rail-enforced call that returns a compact result instead of output
the agent has to read:

- **`warden commit -m "<message>"`** — stages and commits every change on the
  current branch. Refuses protected branches (`main`/`master`), runs pre-commit
  hooks and surfaces **only** a failure, and links the commit to the agent
  record. Returns `{committed, sha, branch, files}`; a clean tree is a no-op.
- **`warden push`** — pushes the current branch to `origin` (sets upstream).
  Refuses to push `main`/`master` directly — push your agent branch and open a PR.
- **`warden sync [--base main]`** — fetches `origin` and rebases the current
  branch onto `origin/<base>`. Refuses a dirty tree (commit first). On a conflict
  it leaves the rebase in progress and reports **only** the conflicting files for
  you to resolve, then `git rebase --continue`.

Pass `--json` to any of them for the raw struct. `git status` / `log` / `diff`
stay yours to run directly — only mutations are routed through warden. Spawned
write-agents get a system-prompt nudge toward these tools (the `git_conventions`
config flag, on by default; `git status/log/diff` stay free).

```sh
warden commit -m "fix the auth redirect loop"
warden push
warden sync --base main
```

### `warden check [name]` (configured project checks)

`warden check` runs the test/lint/build commands a project declares in its
in-repo **`.warden/check.yml`** and returns a compact pass/fail summary — with
captured output for the **failing** checks only, in place of the hundreds of
lines a raw test run spills into the agent transcript. `warden check` runs every
configured check; `warden check <name>` runs one (e.g. `test`, `lint`, `build`).
It exits non-zero when any check fails, and `--json` emits the raw
`{passed, checks:[{name,cmd,passed,exit_code,output}]}`.

The commands come entirely from the project, so warden stays language-agnostic —
a repo with no `.warden/check.yml` has nothing to run (run your tests directly).
The config is the single source of truth shared with the test-redirect hook, so
the gate and the runner can never drift. A check can scope to a sub-directory for
monorepos:

```yaml
# .warden/check.yml
check:
  test:  go test ./...
  lint:  golangci-lint run
  build: go build ./...
  api:                     # monorepo: scoped task
    cmd: go test ./...
    dir: services/api
```

```sh
warden check          # run every configured check
warden check test     # run just the "test" entry
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

### `warden done <TICKET> [--hard] [--create-pr [--base <branch>]]`
Terminate the agent (kill its tmux + claude session) **and** clear its record in
one step — equivalent to `terminate` then `delete`. It does **not** remove the
git worktree; that's a separate, explicitly-confirmed step (`remove-worktree`).

With `--create-pr`, warden first pushes the agent's branch and opens a GitHub PR
(via `gh`) before finishing — the title comes from the agent's subject/task and
the body is its completion digest (files changed + narrative). `--base` sets the
PR target (default `main`). The PR is opened *before* the agent is torn down, so
if it fails (dirty push, protected branch, `gh` missing) the agent is left
running to fix and retry; an existing PR for the branch is reported, not
re-created. Requires the [`gh` CLI](https://cli.github.com) authenticated.

```sh
warden done PROJ-350               # terminate + clear record (worktree kept)
warden done PROJ-350 --hard        # purge the record instead of archiving it
warden done PROJ-350 --create-pr   # push branch + open a GitHub PR, then finish
warden done PROJ-350 --create-pr --base develop
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

### `warden daemon [--addr ADDR] [--log-level LEVEL] [--log-format FORMAT]`
Run the hub (HTTP API + poller). Normally launchd's job; run by hand to debug.

Logging is structured (`log/slog`). `--log-level` is one of `debug | info | warn
| error` (default `info`); `--log-format` is `text` (human-readable, default) or
`json` (one object per line, for log shippers). Both flags override the
`log_level` / `log_format` config keys. Output goes to stderr.

```bash
warden daemon --log-level debug --log-format json
```

### `warden mcp [--addr ADDR]`
Run the MCP stdio server (see §8).

### `warden digest <TICKET> [--json]`
Summarize what an agent accomplished — files touched, branch, turn count, and a
short narrative. Also a web **Digest** panel and the cockpit `d` key.

### `warden approvals` / `warden approve <TICKET> <option>`
With the approvals inbox on (the `approvals` setting, default on), `approvals` lists
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

### `warden pipeline validate|create|start|show|list|cancel|retry|edit-job|delete`
Define and run a **DAG of agent jobs** from a YAML spec (CLI-only authoring). See
§7.5 below for the full guide.

### `warden stats [--watch] [--history [--agent ID]] [--json]`
Warden's resource footprint. Bare, it prints a live snapshot: a system line
(memory/swap/pressure), the daemon's own stats, and per-agent RSS/CPU/procs/
uptime (the memory hog on top). `--watch` redraws every 3s.

`--history` switches to **persisted per-agent performance history** rolled up
from the metrics recorder (requires the `metrics` setting on): runtime, latest/
peak/trend RSS, avg/peak CPU, context-token trend, and changed-file count, with
any anomaly warnings (climbing memory, climbing/critical context, pinned CPU).
`--agent ID` narrows to one agent. `--json` emits the raw structure for either
mode.

```sh
warden stats                      # live snapshot
warden stats --watch              # live, auto-refreshing
warden stats --history            # per-agent history + anomaly warnings
warden stats --history --agent agent-4f2a
```

### `warden doctor`
Preflight checks — required binaries (`tmux`, `git`, `claude`, `gh`), daemon
reachability, and the data directory.

### `warden version [--json]` / `warden --version`
Print the version plus build metadata — commit, build date, Go version, and
platform. Release builds stamp the real tag/commit/date via ldflags; source
builds report `dev` with the commit/date from `make build` (or the embedded VCS
stamp). `--json` emits the same fields as a JSON object for scripting.

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
warden pipeline validate -f review.yaml # check the spec (DAG/refs/cycles); exit 0/1, no daemon
warden pipeline create -f review.yaml   # validate + register (does NOT start)
warden pipeline start <id>              # spawn jobs with no dependencies
warden pipeline show <id>               # jobs, status, branches, emitted output
warden pipeline list
warden pipeline retry <id> <job>        # re-run a failed/needs-attention job
warden pipeline edit-job <id> <job> --prompt "…"   # edit a still-pending job
warden pipeline pause <id>              # stop spawning new jobs (in-flight keep running)
warden pipeline resume <id>             # resume a paused pipeline
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
      "args": ["mcp"]
    }
  }
}
```

`warden mcp` connects to the daemon at the `addr` config setting (default
`127.0.0.1:8765`); to point it elsewhere use `"args": ["mcp", "--addr", "host:port"]`.

Tools exposed:

| Tool | Does |
|---|---|
| `list_agents` | List all agents with status, workdir, subject |
| `get_agent` | Full detail (status, workdir, subject, events, worktree) for one |
| `spawn_agent` | Spawn a new agent — `prompt` for a quick auto-typed one, or `type`+`repo` for a managed worktree |
| `adopt_agent` | Register an existing Claude session: resume newest-for-dir under tmux, or live-register a running tmux session |
| `send_to_agent` | Type a message into a specific agent's claude session |
| `get_agent_output` | Recent terminal output of a specific agent |
| `commit` | Stage+commit the worktree on its branch — rails (no main/master), pre-commit hooks parsed to surface only failures, SHA linked to the agent. Returns `{committed, sha, branch, files}` |
| `push` | Push the current branch to origin (refuses `main`/`master` directly) |
| `sync` | Fetch + rebase onto `origin/<base>` (default `main`); on conflict returns only the conflicting files |
| `check` | Run the project's configured `.warden/check.yml` checks (tests/lint/build); returns pass/fail with output for only the failing checks. `name` runs one, omit to run all |
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

### Isolation guard (auto-injected, no setup)

Separately from the status hooks above, warden installs a **PreToolUse isolation
guard** into every isolated agent automatically — no manual `settings.json`
merge. At spawn it writes a per-agent `claude --settings` file (under
`~/.warden/settings/<id>.json`) that registers a `PreToolUse` hook over the
file-mutating tools (`Edit`/`Write`/`MultiEdit`/`NotebookEdit`). The hook is the
warden binary itself (`warden hook guard`): before each edit it asks the daemon
whether the target path is inside the agent's own worktree. An edit that escapes
into the shared repo root (or a sibling agent's worktree) is **denied** with a
redirect message Claude can act on; everything else passes.

Because it is a per-agent `--settings` file (and `--settings` is *additive*), the
guard applies only to warden-spawned agents — your own Claude sessions are
untouched — and your global status hooks still fire. Like the status hook it
**fails open**: a missing path, unknown session, or unreachable daemon allows the
edit, so the backstop can never wedge an agent. Disable it with
`warden config set isolation_guard false`.

### Git-redirect guard (auto-injected, no setup)

The same per-agent `--settings` file also registers a **PreToolUse hook over
`Bash`** (`warden hook git-guard`) that turns the `git_conventions` *nudge* into
hard enforcement. Before a Bash call runs, the hook argv-parses the command and,
if it is a raw git **mutation** — `git commit`, `git push`, `git pull`, or
`git rebase` — **denies** it with a message naming the warden tool to use instead
(`mcp__warden__commit` / `push` / `sync`, or the `wd` equivalents). It quote-aware
parses the command line, so a mutation named inside a commit message or a quoted
argument is not a false positive, and it walks past `cd … &&`, env prefixes, and
git global flags (`-C`, `-c`, …) to find the real subcommand.

Read-only git stays yours to run directly — `git status`, `log`, `diff`, `show`,
`branch`, `fetch`, `add` and the rest pass straight through. Unlike the isolation
guard this needs no daemon round-trip (the redirect is a static mapping) and it
also **fails open** on unreadable input. Disable it with
`warden config set git_redirect false`.

### Check-redirect guard (auto-injected, config-driven)

A second **PreToolUse hook over `Bash`** (`warden hook check-guard`) does for the
test/lint/build loop what the git guard does for git: before a Bash call runs it
checks whether the command is one the project **registered in `.warden/check.yml`**
— `go test ./...`, `make verify`, `npm test`, … — and if so **denies** it with a
message pointing at `mcp__warden__check` (or `wd check <name>`), which runs the
configured command and returns only the failures instead of the full log (see
[`warden check`](#warden-check-name-configured-project-checks)).

Because test vocabulary is open-ended, the hook **never guesses** — it reads the
same `.warden/check.yml` that drives `wd check` (one config, so the gate and the
runner can't drift) and redirects **only** the commands that file registers. A
command matches when the registered command's leading words are a prefix of it, so
`go test ./...` and `go test ./... -count=1` are redirected but a focused
`go test -run TestX ./pkg` (which `wd check` can't reproduce) is run directly. A
repo with **no config redirects nothing**, so the feature is effectively opt-in
per repo, and the hook **fails open** on unreadable input or a malformed config.
Disable it with `warden config set check_redirect false`.

### Local model (optional, off by default)

The guards above move deterministic work onto warden with **no LLM at all**. A few
remaining responsibilities are fuzzy-but-cheap — the first is **task
classification** (labelling a prompt-spawned agent as `development` / `tests` /
`docs` / …), which warden does today by calling its *own* headless Claude on every
spawn. You can route that to a **local model** instead, so it never touches your
Claude budget:

```sh
warden config set local_llm true                       # off by default
warden config set local_llm_url   http://localhost:11434   # an Ollama-compatible server
warden config set local_llm_model qwen2.5-coder:7b
warden config set local_llm_timeout 20s                # hard per-call cap
```

With `local_llm` on, the daemon routes three fuzzy-but-cheap responsibilities at
the configured Ollama endpoint:

- **Task classification** — labelling a prompt-spawned agent (`Classify`).
- **Activity subjects** — the ≤8-word "currently working on" phrase warden shows
  in `wd ls` and digests (`Summarize`).
- **Oversized check failures** — when a `wd check` command fails and its captured
  output exceeds the line cap, the local model condenses it to the distinct
  failures (the failing test / `file:line` plus the verbatim error) instead of
  spilling a truncated tail into the agent's transcript. The deterministic
  tail-truncation is the fallback, so the agent never loses the failure.

**Every call has a deterministic fallback:** on any error, timeout, or unreachable
server, classification and summaries fall back to headless Claude (classification
then falls back to the `other` label; a check summary falls back to the truncated
tail) — so a stopped or slow Ollama never blocks an agent, it just forgoes the
saving. Local inference on CPU can be slower than calling Claude, hence the hard
`local_llm_timeout`. warden works fully headless without any of this; the local
model only earns its place on these cheap tasks and is never used to decide code
changes or rewrite the operator's intent.

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

**Search the fleet:** the Overview tab has a search box that filters the
all-agents grid live as you type (matching id/name/type/subject/branch and
more), so you can pin down one agent in a crowded grid without scrolling.

**Batch operations (Cockpit):** each tile in the Cockpit grid has a checkbox;
click to select, Shift-click to select a range. While anything is selected a
floating action bar lets you **Message…**, **Terminate**, or **Delete** the whole
selection at once (the destructive actions ask for a second click to confirm).
Agents are processed one at a time and the bar reports partial success, keeping
any failures selected so you can retry them.

**Archive tab:** the 🗄 **Archive** tab browses ended agents from the persisted
`closed/` store. Filter by age (all / 24h / 7d / 30d) and type, plus a free-text
box, to find a past run by ID, name, branch, or subject.

> The UI is baked into the binary at build time. After changing anything under
> `web/`, rebuild (`make release`, or `make ui` for the frontend only) and
> restart the daemon. For live UI iteration, run `warden daemon` and
> `make ui-dev` in parallel and open `http://localhost:4321`.

### Remote access (phone, tablet, another machine)

By default the daemon binds `127.0.0.1:8765` — reachable only from the same
machine, and with **no authentication** (safe on loopback). To reach the
dashboard from another device you change two things: bind a non-loopback
address, and set an access token. The daemon **refuses to start** on a
non-loopback address unless a token is set, so you can't accidentally expose an
unauthenticated daemon.

**1. Generate a token and export it** (it's read from the environment, never
written to the config file, so the secret stays off disk):

```sh
export WARDEN_TOKEN=$(warden token generate)   # 32-byte random hex
```

Put the same `export` in the daemon's service unit (launchd `EnvironmentVariables`
/ systemd `Environment=`) so a background daemon picks it up. Treat the token
like a password; the same value is shared by every client.

**2. Bind a non-loopback address** — either for one run or in the config:

```sh
warden daemon --addr 0.0.0.0:8765      # all interfaces; or addr: 0.0.0.0:8765 in config
```

**3. Open the dashboard from the other device.** On first load (or any time a
request returns `401`) the UI shows an **access-token prompt** — paste the
token; it's stored in `localStorage` and sent on every request. A **🔑 sign
out** control in the top bar forgets it. The local CLI/TUI keep working
transparently: they read `WARDEN_TOKEN` from the same environment.

#### One-shot managed install (recommended)

The install/reinstall scripts automate all of the above for the background
service. Pass a non-loopback `WARDEN_ADDR` and they will mint a token, store it
in `~/.warden/token.env` (`chmod 600`), and wire it into the service unit
(systemd `EnvironmentFile=` / launchd inlined `EnvironmentVariables`, plist
`chmod 600`):

```sh
WARDEN_ADDR=0.0.0.0:8765 ./scripts/reinstall.sh   # or install.sh
```

The token is **generated once and reused** on later installs, so phones/clients
keep working across upgrades. The script prints the token and a shell-rc snippet
so the local CLI/TUI pick up `WARDEN_TOKEN` too. Installing with a loopback
address (the default) provisions **no** token and leaves the service auth-free,
exactly as before.

To retrieve the current token later (e.g. to paste into a new phone), run
`warden token show` — it prints the token local clients resolve (`WARDEN_TOKEN`
if exported, else `~/.warden/token.env`) to stdout, with its source on stderr.

To rotate the token, run `warden token rotate`: it mints a fresh secret, writes
it to `~/.warden/token.env` (`chmod 600`), and restarts the managed service so
the new token is live immediately (on macOS it also rewrites the inlined plist
value). Then re-paste the new token into remote clients and re-export
`WARDEN_TOKEN` in any shell that held the old one. Pass `--no-restart` to stage
the new token without restarting.

#### How to reach it over the network

| Path | Setup | Notes |
|---|---|---|
| **LAN** | `--addr 0.0.0.0:8765`, browse to `http://<host-ip>:8765` | Same WiFi/subnet only. Plain HTTP. |
| **Tailscale** (recommended) | Install on host + device, then `http://<host>.ts.net:8765` | Works anywhere, end-to-end encrypted, stable name. Tailscale's HTTPS/Serve gives you TLS with no cert management. |
| **Cloudflare Tunnel** | `cloudflared tunnel --url http://localhost:8765` | Public HTTPS URL, no port-forwarding. The tunnel runs on the host and forwards over loopback — which is exactly why there is **no loopback exemption**: the token is required on every request. |

> **TLS:** the daemon serves plain HTTP — terminate TLS at the network layer
> (Tailscale HTTPS or Cloudflare Tunnel). Don't expose plain HTTP to the public
> internet; put it behind Tailscale or a tunnel.

**Hardening built in:** the bearer-token check is constant-time, and repeated
failed attempts from one source IP are rate-limited (HTTP `429`) — a valid token
is never throttled. To bind a non-loopback address *without* auth anyway (not
recommended), set `allow_nonloopback: true` in the config.

---

## 11. Configuration

Warden reads all settings from a single YAML file (default `~/.warden/config.yaml`).
Run `warden config init` to generate a fully-commented file, edit the values, then
restart the daemon; `warden config` prints what's live. The `--config <path>` flag
points any command at an alternate file, and `--addr <host:port>` overrides the
daemon address for a single command.

| Setting | Default | Description |
|---|---|---|
| `addr` | `127.0.0.1:8765` | Daemon listen/connect address. A non-loopback address requires `WARDEN_TOKEN` (bearer-token auth) — see [Remote access](#remote-access-phone-tablet-another-machine) — or `allow_nonloopback: true` to bind without auth |
| `data_dir` | `~/.warden` | Directory for warden state: session JSON (`sessions/`, `closed/`), per-agent prompt files (`prompts/`), inbox, pipelines, and metrics |
| `claude_projects_dir` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects and the context gauge |
| `model_default` | `claude-sonnet-4-6` | Default model for new agents (a model id or alias: `sonnet`/`opus`/`haiku`/`fable`) |
| `default_permission_mode` | `auto` | Default permission mode for new agents (`auto`/`default`/`acceptEdits`/`bypassPermissions`/`dontAsk`/`plan`) |
| `notify` | `false` | macOS/libnotify desktop notifications when an agent needs attention |
| `webhook_enabled` | `false` | POST a notification to `webhook_url` for every alert that also goes to desktop `notify` — attention-needed transitions (`waiting_for_input`, `errored`, `orphaned`) and context-size warning/critical alerts. Best-effort and non-blocking |
| `webhook_url` | _(empty)_ | Webhook endpoint POSTed when `webhook_enabled` is on. A Slack incoming-webhook URL works out of the box (the JSON `text` field is what Slack renders); any endpoint accepting `{text, title, body}` works. `warden config` shows this as `(set)`/`(unset)` since a Slack URL embeds a secret token |
| `approvals` | `true` | The approvals inbox: parse recognized tool-permission prompts and surface them for one-click answers in the web/TUI/CLI |
| `token_guard` | `true` | Context-size guard master switch: read each live agent's context-window fill from its transcript, classify `ok`/`warning`/`critical`, and show a state-colored token figure in `ls`/TUI/web |
| `token_warn_alert` | `true` | Fire a desktop notification (when `notify` is on) once per upward crossing into warning/critical |
| `token_auto_compact` | `true` | Auto-send `/compact` when an agent is `critical` and idle/waiting (cooldown-guarded) |
| `token_warn` | `200000` | Warning threshold in context tokens (inclusive). Both thresholds reset to defaults if critical ≤ warn |
| `token_critical` | `400000` | Critical threshold in context tokens (inclusive) — the auto-`/compact` trigger band |
| `log_level` | `info` | Minimum severity the daemon logs (`debug`/`info`/`warn`/`error`). Overridden by `warden daemon --log-level` |
| `log_format` | `text` | Daemon log output format: `text` (human-readable) or `json` (structured, one object per line). Overridden by `warden daemon --log-format` |

`warden config` lists every setting, including `spawn_gate` / `spawn_gate_max_agents`,
`metrics`, `allow_nonloopback`, `auto_approve`, `pipeline_keep_done` / `pipeline_hint`,
the `auto_restart_*` knobs, the `rate_limit_*` knobs (§12.1), and the
`log_level` / `log_format` logging knobs.

> The old `WARDEN_*` configuration environment variables are no longer read — the
> daemon warns once at startup if any are still set. `WARDEN_TOKEN` (the remote-access
> bearer token) is the deliberate exception: it stays an env var so the secret never
> lands in the config file, and it does not trigger the legacy warning. The per-agent
> IPC vars warden injects into each agent (`WARDEN_SESSION_ID`, `WARDEN_PIPELINE_ID`,
> `WARDEN_JOB_ID`) are not configuration and are unaffected.

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

Config settings for the daemon (in `~/.warden/config.yaml`; restart the daemon
after editing):

```yaml
# Disable auto-resume (manual intervention only)
rate_limit_auto_resume: false

# Change retry interval (default: 30m)
rate_limit_retry_interval: 15m

# Change safety buffer after parsed time (default: 1m)
rate_limit_buffer: 2m
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
| `healthz` fails / daemon won't start | Data dir not writable. Check the `data_dir` config setting (default `~/.warden`) and `/tmp/warden.daemon.err`. |
| New agent stuck at `classifying…` / type is `other` | `claude` not on the daemon's PATH. Type falls back to `other`; functionality is otherwise fine. |
| `SUBJECT` stays empty | Poller hasn't refreshed yet (it's throttled and only runs when pane content changes), or the `claude_projects_dir` config setting is wrong. |
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

**Conditional steps** (`run_if:` field) gate a job on its upstream outcomes:

| Value | Job runs when… |
|---|---|
| `success` (default) | every dependency succeeded; skipped if any failed (today's behaviour) |
| `failure` | at least one dependency **failed** — for cleanup/notify/rollback steps |
| `always` | all dependencies have finished, regardless of success or failure |

A job is decided only once all its dependencies have settled. A `failure`/`always`
handler is told which upstream failed (the failed job has no handoff of its own),
so it can react. When a failure has such a downstream handler the pipeline is not
considered stalled — it keeps running, and completes `done` if the handler
succeeds.

```yaml
jobs:
  - id: deploy
    prompt: "Deploy the release."
  - id: rollback
    depends_on: [deploy]
    run_if: failure        # only runs if deploy failed
    prompt: "Roll back the deploy."
  - id: notify
    depends_on: [deploy]
    run_if: always         # runs either way
    prompt: "Post the deploy outcome to the channel."
```

**Failure behaviour:** if a job's agent session enters `errored` or `orphaned`,
the job is marked `failed`. Its pending `success` descendants are marked
`skipped`; any `failure`/`always` handler downstream instead runs. With no
handler the pipeline status becomes `stalled`. Jobs that were already running are
not interrupted. A `stalled` pipeline can be inspected with `pipeline show` and
cleaned up with `pipeline cancel`.

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
