# Using warden

A practical, task-oriented guide to running `warden` day to day. For build,
install, and contributor details see the [README](../README.md); for a complete
catalog of what warden can do see [FEATURES.md](FEATURES.md); this document
focuses on **how to use the tool once it's installed**.

> `alias agents=warden` is handy (and a built-in `wd` symlink aliases `warden`) — every command below works under either name.

---

## 1. What warden is (the mental model)

`warden` (aliased as `wd`) lets you run many **coding-agent sessions** in parallel and
watch them from one place. Each agent is a real agent process (a `claude` process by
default) running inside its own detached **tmux** window. warden drives multiple
agent backends — see [Agent backends](../README.md#agent-backends---backend). You
spawn agents, watch what they're doing, talk to them, and tear them down — without
juggling terminals by hand.

One binary wears several hats:

| Face | What it is | You run it… |
|---|---|---|
| **daemon** | The single long-running process. Owns the on-disk session store, serves a loopback REST API on `127.0.0.1:8765`, and runs a background poller that keeps each agent's status and subject fresh. | Once, in the background (usually via launchd). |
| **CLI client** | `ls`, `status`, `start`, `done`, `attach`, `send`, `tail`, the git/check lifecycle verbs (`commit`/`push`/`sync`/`check`), and more — thin HTTP clients that talk to the daemon. | Whenever you want to act on agents. |
| **TUI cockpit** | `warden tui` (or bare `warden`) — a live tmux-based terminal dashboard of the whole fleet. | When you want a terminal cockpit. |
| **Web GUI** | A React dashboard the daemon embeds and serves alongside the API — tabbed mission control with live SSE, interactive terminals, and an attention queue. | Open the daemon's address in a browser. |
| **MCP server** | `warden mcp` — a stdio bridge so an *orchestrator* agent session (e.g. Claude) can manage agents through tool calls. | Wired into an orchestrator agent's MCP config. |
| **Interactive REPL** *(experimental)* | `warden repl` — a local-LLM conductor REPL that turns plain-English intent into confirmed warden actions, spending no cloud-model tokens. | When you want NL control without an MCP orchestrator session. |

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
ollama --version     # optional — only for local_llm / `wd repl`
curl -s localhost:8765/healthz   # → {"status":"ok"} means the daemon is up
```

`warden doctor` runs these binary checks for you (plus daemon + data-dir
probes), and `warden setup` installs whatever is missing — see §“Preflight &
setup” below.

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
curl -s localhost:8765/healthz         # {"status":"ok"}
# macOS (launchd) — logs are files under /tmp:
tail -f /tmp/warden.daemon.log         # stdout
tail -f /tmp/warden.daemon.err         # stderr
# Linux (systemd) — logs go to the journal, not /tmp:
journalctl --user -u warden -f         # stdout + stderr, follow live
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

When spawning agents via the MCP `spawn_agent` tool (from an orchestrator agent session), pass the `model` parameter:

```typescript
// Orchestrator agent calls spawn_agent
spawn_agent({
  prompt: "Analyze the auth module",
  model: "opus"  // or "claude-opus-4-8"
})
```

The same default resolution applies: the `model_default` config setting → `claude-sonnet-4-6`.

### Restored agents

When you `warden restore <id>` an orphaned agent, it resumes with the **original model** it was spawned with — the model is stored in the session record and preserved.

---

## 5.2. Agent backends (`--backend`)

Warden drives **Claude Code** by default, but the agent layer is an **adapter
layer**: each console coding agent is normalized behind a `Backend` interface, and
you pick one per agent at spawn time.

> **Important:** warden is fully tested only with **Claude Code**. All non-`claude`
> backends (Aider, OpenCode, Codex, Crush, Goose, Cursor, Antigravity) are
> **experimental / work-in-progress** — functionality may be reduced or unverified.
> Any non-`claude` value for `--backend` is experimental.

| Agent | Status |
|---|---|
| Claude Code | ✅ Stable — fully tested, reference backend |
| Aider | 🧪 Experimental (WIP) |
| OpenCode | 🧪 Experimental (WIP) |
| Codex CLI | 🧪 Experimental (WIP) |
| Crush | 🧪 Experimental (WIP) |
| Goose | 🧪 Experimental (WIP) |
| Cursor CLI | 🧪 Experimental (WIP) |
| Antigravity CLI | 🧪 Experimental (WIP) |

| Backend | `--backend` | Tier | What works / degrades |
|---|---|---|---|
| **Claude Code** (default) | `claude` | A | Everything — digests, savings, priced spend, resume, all permission modes |
| **Aider** | `aider` | A | 🧪 Experimental. Bring-your-own-model (pass `--model`); structured markdown transcript ⇒ real digests. **No** resume (rotate/handoff re-spawn fresh), **no** priced spend (`wd spend` shows tokens, `wd savings` omits it), no assignable session id, system-prompt hints skipped. Runs an autonomous `--message` task that exits when done. |
| **OpenCode** | `opencode` | A | 🧪 Experimental. Bring-your-own-model (pass `--model`, e.g. `ollama/qwen2.5-coder:3b`); structured JSON transcript (sourced via `opencode export`) ⇒ real digests. **Resumes** the worktree's last session (`opencode -c`, dir-scoped), so rotate/handoff/restore work. **No** priced spend (`wd spend` shows tokens, `wd savings` omits it — BYO model), no warden-assigned session id, system-prompt hints skipped. Runs a persistent agent loop (TUI, prompt seeded via `--prompt`). |
| **Codex CLI** | `codex` | A | 🧪 Experimental. BYO provider (via `~/.codex/config.toml`; pass `-m` for model); structured JSONL transcript (rollout files) ⇒ real digests. **Resumes** dir-scoped (`codex resume --last`), upgraded to exact-id via discover-then-pin (`DiscoverSessionID`). Live state + approval detection; context injection via `AGENTS.md` (`InjectContext`). No priced spend (tokens-only). See [`docs/agent-backends/codex.md`](../agent-backends/codex.md). |
| **Crush** | `crush` | A | 🧪 Experimental. BYO model (TUI is config-driven; headless `crush run` accepts `-m`); structured JSON transcript (via `crush session show --json`) ⇒ real digests. **Resumes** dir-scoped (`crush --continue`). **TUI takes no initial prompt** — type it after attach. Context injection via `CRUSH.md` (`InjectContext`); no TUI approval parsing yet. No priced spend. See [`docs/agent-backends/crush.md`](../agent-backends/crush.md). |
| **Goose** | `goose` | A | 🧪 Experimental. BYO provider (`GOOSE_PROVIDER`/`GOOSE_MODEL` env); structured JSON transcript (via `goose session export`) ⇒ real digests. **Resumes** name-deterministic (`goose session -r --name <id>`). No model flag on session launch. Context injection via `.goosehints` (`InjectContext`); no TUI approval parsing yet. No priced spend. See [`docs/agent-backends/goose.md`](../agent-backends/goose.md). |
| **Cursor CLI** | `cursor` | C | 🧪 Experimental. Hosted Cursor plan (`cursor-agent login`); rich native permission modes (`plan`/`ask`/`auto-review`/`force`). **Resumes** dir-scoped (`--continue`). Live state + command-allowlist/workspace-trust approval detection; context injection via `AGENTS.md` (`InjectContext`). **No structured transcript yet** (interactive `store.db` is unreadable) ⇒ no digests; no priced spend. See [`docs/agent-backends/cursor.md`](../agent-backends/cursor.md). |
| **Antigravity CLI** | `antigravity` | A | 🧪 Experimental. Google-hosted free tier (`agy`, multi-vendor model menu); structured trajectory JSONL (incl. tool calls / files changed) ⇒ real digests. **Resumes** dir-scoped (`agy -c`). Live state + `Do you want to proceed?` approval and workspace-trust detection; context injection via `AGENTS.md` (`InjectContext`). No priced spend. See [`docs/agent-backends/antigravity.md`](../agent-backends/antigravity.md). |

```sh
# Claude (default) — nothing to pass
warden start "review the auth module"

# Aider against a local Ollama model (free, offline, $0)
export OLLAMA_API_BASE=http://127.0.0.1:11434
warden start "implement the add function" \
  --backend aider --model ollama_chat/qwen2.5-coder:3b --dir .

# OpenCode against a local Ollama model (free, offline, $0)
warden start "implement the add function" \
  --backend opencode --model ollama/qwen2.5-coder:3b --dir .

# Codex — configure provider in ~/.codex/config.toml first
warden start "implement the add function" --backend codex --dir .

# Crush — configure provider in ~/.config/crush/crush.json first
warden start "implement the add function" --backend crush --dir .

# Goose against a local Ollama model ($0)
GOOSE_PROVIDER=ollama GOOSE_MODEL=qwen2.5-coder:3b \
warden start "implement the add function" --backend goose --dir .
```

Over MCP, pass the `backend` param (kept at parity with the CLI):

```typescript
spawn_agent({ prompt: "implement add", backend: "aider", model: "ollama_chat/qwen2.5-coder:3b" })
spawn_agent({ prompt: "implement add", backend: "opencode", model: "ollama/qwen2.5-coder:3b" })
spawn_agent({ prompt: "implement add", backend: "codex" })
spawn_agent({ prompt: "implement add", backend: "crush" })
spawn_agent({ prompt: "implement add", backend: "goose" })
```

An unknown backend id is rejected up front (before any tmux/worktree side effect).
The selected backend is stored on the session (`Session.Backend`; empty ⇒ claude,
so existing stores are unaffected). Capabilities differ per backend and warden
**degrades gracefully** rather than crashing when one is missing (design §5).

---

## 5.3. Agent roles (`--role`)

A **role** is a named, persistent system-prompt **persona** attached to an agent,
plus a set of default spawn flags. Every agent has exactly one role; the default
is `general`, which injects no persona and behaves exactly as agents did before
roles existed. The role set is a **fixed built-in catalog** — there are no
user-defined roles.

### The built-in roles

```sh
warden role list
```

| Role | Persona (summary) | Default flags |
|---|---|---|
| `general` | *(none — a plain agent)* | — |
| `orchestrator` | Coordinates a fleet of warden agents to deliver a goal — plans and delegates via the warden MCP/CLI, doesn't write feature code itself unless trivial. | `--permission-mode auto` |
| `implementer` | Implements a task end-to-end on its own branch: code, tests, project checks, commit, PR. | `--type development` |
| `auto-merger` | Owns getting an open PR merged — monitors CI, fixes failures/conflicts, merges when green and approved. | `--permission-mode auto`, auto-approve on |
| `reviewer` | Reviews a branch/PR for correctness, coverage, and style; produces findings + a merge/blocker verdict, no fixes unless asked. | `--type pr-review` |

### Choosing a role at spawn

```sh
# Attach a role at spawn; its default flags fill any you leave unset
warden start "review PR 1234 for correctness" --role reviewer

# An explicit flag always overrides the role's default
warden start PROJ-9 --role implementer --type spike   # spike wins over the role's development default
```

The role's defaults follow **precedence: explicit request value > role default >
global default**. Default `tags` are **unioned** into whatever tags you passed
(deduplicated), never replacing them; `auto_approve` is OR-ed in.

Roles are also selectable in the UIs — the TUI new-agent form has a `ctrl+r` role
picker, and the web **+ New agent** modal a **Role** dropdown (both default to
`general`) — and over MCP via `spawn_agent`'s `role` param.

### Switching a running agent's role

```sh
warden set-role agent-abc123 reviewer   # give the running agent the reviewer persona
warden set-role agent-abc123 general    # clear the persona (back to a plain agent)
```

`set-role` persists the new role **name** and **relaunches** the agent so the
persona takes effect — a persona only injects at (re)launch, so unlike
`set-permission-mode` this discards the agent's in-flight turn. MCP equivalent:
`set_role {ticket, role}`.

### How it's stored and injected

Only the role **name** is persisted (`Session.Role`; empty ⇒ `general`, so
pre-roles stores need no migration). The persona is re-resolved from the embedded
`internal/role` registry at every (re)launch — nothing persona-shaped is ever
written to disk, so switching a role and resuming re-injects the new persona
automatically. Injection reuses the **same seam** warden already uses for its
collab/git/pipeline hints: Claude gets it via `--append-system-prompt` (file-backed
under `HintsDir`, never inlined on the tmux launch line so the 1024-byte MAX_CANON
limit is safe); the injecting backends (Codex/OpenCode/Cursor/Antigravity/Crush/Goose)
get it prepended into their rules-file drop (`AGENTS.md` etc.); Aider, which has no
injection seam, degrades silently like the other hints. An empty persona injects
nothing, leaving a plain/`general` spawn byte-identical to before roles existed.

---

## 6. Command reference

All commands accept `--addr` to point at a non-default daemon (overrides
the `addr` config setting). `<TICKET>` is the agent ID — a Jira key for managed agents,
or an `agent-xxxx` ID for prompt-spawned ones. Agent-addressed commands
(`stop`/`terminate`/`delete`/`remove-worktree`/`done`/`restore`/`send`/`digest`/`create-pr`)
resolve by **either** the agent's `name` **or** its id — the same identifiers `warden ls` shows.

### `warden` / `warden tui`
Open the tmux-composited cockpit (see §7). Bare `warden` with no
subcommand does the same thing. Requires tmux (see §7). Launched from **inside**
an existing tmux session it lays out as a native window in that session instead
of nesting (`--tmux-native`, auto-enabled when `$TMUX` is set — see §7
"Launching from inside an existing tmux session").

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
| `--role` | Built-in agent role: `general` (default, no persona) / `orchestrator` / `implementer` / `auto-merger` / `reviewer`. Injects the role's persona and fills its default flags for any left unset (see §5.3). |

### `warden role list` / `warden set-role <id> <role>`
List the built-in role catalog (name + description), or switch a running agent's
role. `set-role` persists the name and **relaunches** the agent so the new persona
re-injects (its in-flight turn is discarded); `general`/`""` clears the persona.
See §5.3 for the full role model.

```sh
warden role list
warden set-role agent-abc123 reviewer
```

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
- **`warden push [--force-with-lease]`** — pushes the current branch to `origin`
  (sets upstream). Refuses to push `main`/`master` directly — push your agent
  branch and open a PR. After a rebase or amend, `--force-with-lease` overwrites
  your remote branch; warden only ever uses `--force-with-lease` (never a bare
  `--force`), so the push aborts if a teammate pushed to your branch since your
  last fetch. Returns `{branch, remote, pushed, forced}`.
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
warden push --force-with-lease   # after a rebase/amend (safe force)
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

### `warden review [--base <branch>] [--prompt <text>] [--backend <id>] [--json]` (agent-native diff review)

`warden review` asks the agent's **backend** to review its OWN diff — the
agent-native counterpart to `warden check`. Where `warden check` runs the
project's configured test/lint commands and a `pr-review` agent stands up a whole
reviewer session, `warden review` execs the backend's own one-shot reviewer
(**Codex**: `codex review`) in the agent's worktree and streams the findings to
you. Like a check it runs **locally with no daemon round-trip** (so it is
CLI-only), and the model/provider comes from the backend's own config — the
$0-local Ollama rig and a paid setup both work unchanged.

By default it reviews the uncommitted working tree (staged + unstaged +
untracked); `--base <branch>` reviews the branch's changes against a base instead;
`--prompt` adds extra instructions; `--backend` targets a specific backend
(default: the current agent's).

`--json` emits a machine-readable result: warden runs the backend's structured
review (Codex: `codex exec review`), normalizes the backend's NATIVE output into a
neutral findings shape and prints it to stdout (backend progress goes to stderr):

```json
{
  "summary":  "overall human-readable explanation",
  "verdict":  "the backend's overall verdict (codex: overall_correctness)",
  "findings": [
    { "file": "internal/x.go", "line": 42, "severity": "error", "title": "…", "message": "…" }
  ]
}
```

Review quality rides the backend's configured model — a tiny local model may
report no findings; the operator's real model is where this earns its keep.
Backends without a native reviewer (e.g. Claude) are **not offered the verb** — it
exits non-zero pointing you at `warden check` or a `pr-review` agent.

```sh
warden review                       # review my uncommitted changes, stream findings
warden review --base main           # review this branch against main
warden review --json                # neutral machine-readable findings
warden review --backend codex --json
```

### `warden models [--backend <id>] [--json]` (live backend model menu)

`warden models` lists the **live, currently-available model menu** the agent's
backend exposes — the agent-native counterpart to warden's static `opus`/`sonnet`/
`haiku`/`fable` aliases. Backends whose model set is a runtime, multi-vendor menu
implement it: **Antigravity** (`agy models` — Gemini/Claude/GPT-OSS variants) and
**Cursor** (`cursor-agent --list-models`). The ids print one per line (or `--json`
for an array) and feed `--model` verbatim. Listing is a metadata read (the
backend's own list subcommand, not a generation request), so it spends no
hosted-tier quota; like `warden review` it runs locally (CLI-only). Backends with
a static model set (e.g. Claude) have no live menu and exit non-zero pointing you
at `--model` with a known id.

```sh
warden models                       # the current agent's backend menu, one id per line
warden models --backend antigravity # e.g. Gemini 3.5 Flash (Low), Claude Opus 4.6 (Thinking), …
warden models --backend cursor --json
```

### `warden memory [--raw] [--path] [--edit]` (project memory)

warden owns one committed, backend-neutral **project memory** — `.warden/memory.md`
(beside `.warden/check.yml`), keyed implicitly by the repo root (`git rev-parse
--show-toplevel`, auto-created on first use, no `wd init`). It holds durable
cross-agent facts — where things live, how to run X, project invariants — so the
**next** agent (any backend) doesn't re-pay the rediscovery tax. `warden memory`
prints the resolved path and the budgeted, navigational render; `--raw` prints the
file verbatim, `--path` prints just the path (scriptable, no auto-create), `--edit`
opens it in `$EDITOR`. Like `warden check`/`warden review` it is CLI-local (no
daemon round-trip, no MCP twin).

The curated file is **projected into every spawned agent** via the same system-prompt
seam the collab/pipeline/git hints ride — **Claude** via `--append-system-prompt`
(file-backed, so it never bloats the launch line), **codex/cursor/opencode/antigravity**
via their `AGENTS.md` warden block, **crush** via `CRUSH.md`, **goose** via
`.goosehints`; **aider** degrade-skips (neither seam). 7/8 backends project with zero
new adapter code. Config-gated by `memory.inject` (default on); off, or an empty/absent
file, is byte-identical to no injection. warden READS but never rewrites your
CLAUDE.md/AGENTS.md/CONVENTIONS.md — `.warden/memory.md` is warden's own.

You curate it **by hand** (`--edit`), and warden can optionally **auto-propose**
entries behind `memory.curate` (default **off**). When enabled, a debounced pass on
the completion-digest hook extracts durable facts from finished agents and writes
**`unverified`, timestamped, provenance-tagged** proposals — to the **working tree
only**. It **never commits or pushes**, so the committed diff is the human review
gate; proposals promote to `trusted` only on corroboration, contradictions supersede
(tombstone) older entries, un-recorroborated entries age out, and vanished paths are
flagged stale. It prefers the `$0` local model. See the
[Project memory](https://srjn45.github.io/warden/concepts/project-memory/) concept
page.

In `warden repl` you can also **ask** this memory a question — `/memory <q>` (`/mem`,
`/ask`), or the model-callable `project_memory` tool — and warden answers "where does
X live?" / "how do I run Y?" **locally** from `.warden/memory.md` (config-gated by
`memory.ground`, default on). Unlike projection (which *adds* input tokens), grounding
*removes* a cloud round-trip: it is served on the **local model only**, so it is
structurally `$0` and never escalates to a paid model; with no local model configured
it degrades to returning the matching entries verbatim. It is read-only (an
absent/empty file answers "not in project memory", never auto-created) and cites each
entry's trust (`unverified`/`trusted`/`human`) + provenance so a stale hint reads as a
hint.

```sh
warden memory                       # path + the rendered view injected into agents
warden memory --raw                 # the file verbatim
warden memory --edit                # open in $EDITOR (auto-creates it first)
warden memory --path                # just the resolved path (scriptable)
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

### `warden recover [--apply] [--json]`
Safety net for the tombstone reaper: scans **archived** records for ones whose
tmux session is confirmed still alive (a stale `orphaned` status — see
[§12](#12-status-values-youll-see) — racing a daemon restart could previously
let one get archived out from under a live session). Bare `warden recover`
only reports candidates; `--apply` re-inserts
each one into the active store under its original id, so any children
(linked via `parent_id`, untouched by archiving) reconnect automatically.

```sh
warden recover                # report candidates only (dry run)
warden recover --apply        # actually revive them
```

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
`log.level` / `log.format` config keys. Output goes to stderr.

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

### `warden pipeline validate|create|list-templates|start|show|list|cancel|retry|edit-job|delete`
Define and run a **DAG of agent jobs** from a YAML spec or a built-in template. See
§7.5 below for the full guide.

### `warden schedule create|list|delete`
Fire an agent or a pipeline on a timer — **opt-in**, set `scheduler_enabled: true`
in the config file and keep the daemon running (schedules only fire while it is up).
`--cron` is recurring; `--at` is single-shot (fires once, then goes inactive).
Default fire mode is one agent spawn; pass `--pipeline <spec.yaml>` to fire a
pipeline instead. The startup reconcile never backfills a cron run missed while the
daemon was down. See [FEATURES.md §28](FEATURES.md).

```bash
# Recurring: review pending PRs every weekday at 9am
warden schedule create daily-review --cron "0 9 * * *" \
    --type pr-review --repo ~/dev/warden --prompt "Review any open PRs"

# Single-shot: kick off a development agent at a specific time
warden schedule create launch --at 2026-06-27T09:00 \
    --type development --repo ~/dev/warden --prompt "Start the migration"

# Recurring pipeline
warden schedule create nightly --cron "0 2 * * *" --pipeline ci.yaml

warden schedule list
warden schedule delete daily-review
```

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

### `warden cost [spend|savings]`
One umbrella over warden's two financial views — **spend** (real dollars agents
billed Claude) and **savings** (tokens, and the dollars they represent, warden kept
out of context). With no subcommand it prints a combined at-a-glance summary (a
**SPEND** section and a **SAVINGS** section). `warden cost spend` and `warden cost
savings` are the same commands as the top-level `warden spend` / `warden savings`,
with every flag wired through. The top-level commands remain available as aliases.

```sh
warden cost                       # combined: SPEND section + SAVINGS section
warden cost spend --by agent      # same as `warden spend --by agent`
warden cost savings --benchmark   # same as `warden savings --benchmark`
```

Resource footprint (memory/CPU/pressure) is a different axis — see `warden stats` —
and is deliberately not folded into `warden cost`.

### `warden savings [--benchmark] [--since W] [--json] [--audit] [--calibrate]`
Read back the **token-savings ledger** — a real, append-only record of the tokens
warden's lifecycle features (starting with `wd check`) kept out of agents' context
windows. Two axes are reported separately and never blended: the **context** axis
(how much leaner context stayed, % and $) and the **offload** axis (Claude work
moved off entirely onto the local LLM, $). Gated by the `savings` setting (default
on); `GET /api/v1/savings` returns 403 when off. See [FEATURES.md §29](FEATURES.md).

```sh
warden savings                    # per-feature table (saved/raw tokens, events)
warden savings --benchmark        # headline A/B: without-vs-with warden, % cut, $ saved, trend sparkline
warden savings --since 7d         # window (24h/7d/2w) or a date
warden savings --json             # structured summary
warden savings --audit            # raw-vs-kept provenance samples (requires savings_samples)
warden savings --calibrate        # measure this workload's bytes/token vs Claude count_tokens (needs ANTHROPIC_API_KEY)
```

Every figure states its **basis** — `CALIBRATED` (workload-measured via
`count_tokens`) or the generic 4-bytes/token `HEURISTIC`. Calibration is
forward-only: it prices events recorded after it runs.

> **Dollar figures are estimates** based on published list prices (as of 2026-06);
> they exclude prompt-cache tokens and any volume/batch/enterprise discounts, so
> they may differ from your actual bill. Token counts are exact (read from the
> transcript).

### `warden spend [--by agent|repo|day] [--json]`
Report the measured model spend warden read from agents' transcripts — the exact
input/output tokens priced per model into estimated dollar figures and rolled up
per-agent, per-repo, and per-day (dollar pricing currently covers the Claude
backend; bring-your-own-model backends report tokens only). The cost counterpart to `wd savings`. Gated by
the `savings` setting; `GET /api/v1/spend` returns 403 when off. See
[FEATURES.md §30](FEATURES.md).

```sh
warden spend                      # total / today / this week, then per-agent/repo/day $ tables
warden spend --by agent           # show just one rollup: agent, repo, or day
warden spend --json               # structured rollup
```

A **budget gate** (`tokens.budget_gate`, off by default) turns this into a guardrail:
with `tokens.budget_daily_usd` / `tokens.budget_weekly_usd` set, a spawn that would push
measured spend over the cap warns first (re-run with `--force` to proceed),
mirroring the memory-pressure spawn gate. `warden ls` also gains a **COST** column.

> **Dollar figures are estimates** based on published list prices (as of 2026-06);
> they exclude prompt-cache tokens and any volume/batch/enterprise discounts, so
> they may differ from your actual bill. Token counts are exact.

### `warden branches [--json]`
Opt-in, read-only view of each active agent's branch health: its **GitHub CI
status** (latest `gh run list` in the worktree) and its **standing vs `origin/main`**
(commits behind/ahead, merged?). The daemon monitor behind it (enable with
`branch_track.enabled`, tune `branch_track.interval`) delivers **non-blocking**
alerts — an inbox note (+ desktop ping) on a new CI failure, an inbox nudge on a
merged or far-behind branch. Every `gh`/git call fails open. Also at
`GET /api/v1/collab/branches` and the `get_branch_status` MCP tool. See
[FEATURES.md §6](FEATURES.md).

### `warden insights [--json]`
Mine archived agent history for **patterns** — recurring task shapes,
slow/failure-prone work, and parallelization opportunities — as a deterministic
report, optionally narrated by the local LLM (`local_llm`). Gated by `insights`
(default on); also the `insights` MCP tool. See [FEATURES.md §25](FEATURES.md).

### `warden snapshot create|list|restore`
Checkpoint an agent's **worktree changes + session transcript** and roll back
later. Gated by `snapshots` (default on); also the `snapshot_*` MCP tools.

```sh
warden snapshot create [name] -m "before risky refactor"   # capture a checkpoint
warden snapshot list [name] [--all]                        # list checkpoints
warden snapshot restore <id> [--force]                     # re-apply onto its worktree
```

Restore reapplies the captured stash onto the recorded worktree; it refuses a
dirty/conflicting tree rather than clobbering, and a failed apply leaves the
snapshot intact. See [FEATURES.md §23](FEATURES.md).

### `warden tutorial [--skip] [--reset]`
A guided first-run walkthrough of the core loop (spawn → watch → commit → tear
down). Until you've taken or skipped it, warden prints a single non-blocking stderr
hint (suppressed for piped/non-interactive use). `--skip` marks it complete without
running; `--reset` clears the marker so the tour and hint return. Disable the hint
entirely with `tutorial: false`. See [FEATURES.md §24](FEATURES.md).

### `warden plugin list`
Inspect the **plugin** registry — external executables that extend warden with
custom agent task types and lifecycle hooks over a versioned JSON-over-stdio
protocol. **Default off** (`plugins: true` to enable). `list` shows registered
plugins, their paths, declared custom task types (with isolation policy), subscribed
hook events, and any config errors. Hooks are advisory and fail-open — a missing,
slow, or crashing plugin is logged and skipped, never blocking an agent. Configure
via `plugins.enabled` + a `plugins.registry` list; a worked example lives under
`examples/plugins/`. See [FEATURES.md §26](FEATURES.md).

### `warden doctor`
Preflight checks — required binaries (`tmux`, `git`, `claude`), optional ones
(`gh`, `ollama`, warn-only), daemon reachability, and the data directory.

### `warden setup [--yes]`
Verifies the install with the **same checks as `doctor`**, then installs whatever
is missing. Idempotent — it only touches deps that aren't already on PATH. For
each missing dependency it prints the exact install command and prompts before
running it; `--yes` installs everything missing without prompting (for
automation). Required deps (`tmux`, `git`, `claude`) are offered first, then the
optional ones (`gh`, `ollama`).

Package managers are auto-detected: Homebrew on macOS (never auto-bootstrapped —
if `brew` is missing, setup prints the instruction and skips brew installs) and
`apt`/`dnf`/`pacman` on Linux. Claude Code and Ollama use their official
installers (`curl … | bash` / `curl … | sh`). After installing, setup re-runs
the checks and prints a doctor-style report.

`setup` is **CLI-only by design** — it installs host packages, so it is not
exposed over MCP or the daemon.

```sh
warden setup            # confirm-each install of anything missing
warden setup --yes      # non-interactive: install all missing deps
```

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
| `←`/`→` or `h`/`l` | Collapse / expand the pipeline or agent sub-tree under the cursor |
| `Enter` | Open the selected agent (or running pipeline job) in the right detail pane — a finished agent or tombstone shows its stored detail instead of attaching |
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

Agents spawned by another agent (via the `spawn_agent` MCP tool) **nest under
their parent** as a collapsible sub-tree — a `▸ / ▾` header indented per depth,
toggled with `h`/`l` (`←`/`→`), the same affordance pipelines use. Deleting a
parent that still has live children keeps it as a muted **terminated tombstone**
header (`terminated · N running`) with no terminal/attach pane, so the children
never orphan; the daemon reaps the tombstone once the whole sub-tree goes
terminal (reconfirming the tombstone's tmux is actually dead first — an
`orphaned` status alone isn't proof; `warden recover` is the fallback if a
live one is ever archived anyway). `Enter` on a finished agent or tombstone
opens its stored detail instead of attaching to a dead session.

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

Press **Alt+t** to toggle this slot between the master session and a shell. The
shell is created on first use and both keep running across toggles — switching
back and forth never loses the conversation or the shell's scrollback. Exit the
shell (`exit` / Ctrl-D) and the next **Alt+t** starts a fresh one.

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

### Launching from inside an existing tmux session

If you run `warden tui` (or bare `warden`) **from inside a tmux session**, warden
detects `$TMUX` and lays the cockpit out as a **native tmux window** in your
*current* session (named `warden-cockpit`) instead of building its own session to
attach to — a plain `tmux attach` refuses to nest, and nesting two tmux sessions
means dueling prefix keys and status-bar confusion. The native window uses your
own tmux's keybindings, copy-mode, and resizing directly; there is no inner/outer
prefix conflict because it is all one session:

- Its two panes (the **list** on the left, the **detail** on the right) are real
  tmux panes — navigate them with your usual tmux keys (`Ctrl-b ←/→`, `Ctrl-b o`).
- **Enter** opens the selected agent live in the detail pane; **`a`** zooms it
  full-screen (`Ctrl-b L` returns you to the cockpit window).
- **`q`** closes only the cockpit *window* — your other tmux windows and the
  session itself are left untouched.

warden prints a one-line notice when it does this. To force the classic
own-session cockpit instead (e.g. for a screen recording), unset `$TMUX`:
`env -u TMUX warden tui`. You can force the native window explicitly with
`warden tui --tmux-native` (it requires `$TMUX`).

> The native window is intentionally leaner than the classic cockpit: it drops
> the dedicated master shell/REPL pane (your own tmux already gives you shells a
> keypress away with `Ctrl-b c`) and the extra `Alt`-arrow / `Alt-t` bindings (so
> it never touches your personal tmux config). Everything else — the live list,
> new-agent form, approvals, digests, full-screen attach — works the same.

### Requirements

The cockpit requires **tmux ≥ 3.1** — it composites real tmux panes. There is no
single-pane fallback: from a plain terminal (not inside tmux) it builds its own
session and attaches; if tmux isn't installed at all it exits with an error.

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
warden pipeline list-templates          # built-in starters + their placeholders
warden pipeline create -f review.yaml   # validate + register (does NOT start)
warden pipeline create --template analyze-implement-review --set TASK="…"  # from a template
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

**Templates** — skip hand-writing a spec for common shapes. `warden pipeline
list-templates` shows the four bundled starters and the placeholders each needs:

| Template | Shape | Placeholders |
|---|---|---|
| `analyze-implement-review` | analyze → implement → review | `TASK` |
| `parallel-tasks` | two independent tasks → integrate | `TASK_A`, `TASK_B` |
| `test-fix-verify` | reproduce → fix → verify | `TASK` |
| `research-synthesis` | breadth + depth research → synthesize | `TOPIC` |

`create --template <name>` renders the spec and registers it. `{{NAME}}` defaults
to the template name (override with `--name`) and `{{REPO}}` to the current
directory (override with `--repo`); fill every other placeholder with `--set
KEY=VALUE` (repeatable):

```sh
warden pipeline create --template analyze-implement-review \
  --name auth-refactor --repo ~/dev/app \
  --set TASK="extract the session store behind an interface"
warden pipeline start auth-refactor
```

A template with an unfilled placeholder fails fast, naming the missing key.

---

## 8. Orchestrating agents from another agent session (MCP)

Register `warden mcp` as an MCP server in your *orchestrator* agent session
(e.g. Claude) so it can manage agents via tool calls. Add to its MCP config — for a
Claude Code orchestrator that's `~/.claude/claude_desktop_config.json` or a project
`.claude/mcp.json`; other MCP-capable agents use their own config path:

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
| `push` | Push the current branch to origin (refuses `main`/`master` directly); `force: true` uses `--force-with-lease` |
| `sync` | Fetch + rebase onto `origin/<base>` (default `main`); on conflict returns only the conflicting files |
| `check` | Run the project's configured `.warden/check.yml` checks (tests/lint/build); returns pass/fail with output for only the failing checks. `name` runs one, omit to run all |
| `terminate_agent` | Stop an agent (kill tmux + claude); keeps record + worktree. Reversible via `restore_agent` — the default "stop" action |
| `restore_agent` | Recreate and resume a lost/orphaned agent (`claude --resume`) |
| `recover_agents` | Safety net for the tombstone reaper: revive archived records whose tmux session is confirmed still alive. `apply:false` (default) only reports candidates; `apply:true` re-inserts each one under its original id |
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
edit, so the backstop can never wedge an agent. Disable it by setting
`rails.isolation_guard: false` in your config file.

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
also **fails open** on unreadable input. Disable it by setting
`rails.git_redirect: false` in your config file.

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
Disable it by setting `check_redirect: false` in your config file.

### Local model (optional, off by default)

The guards above move deterministic work onto warden with **no LLM at all**. A few
remaining responsibilities are fuzzy-but-cheap — the first is **task
classification** (labelling a prompt-spawned agent as `development` / `tests` /
`docs` / …), which warden does today by calling its *own* headless Claude on every
spawn. You can route that to a **local model** instead, so it never touches your
Claude budget:

```yaml
# in ~/.warden/config.yaml (run `wd config path` to locate it), then restart the daemon
local_llm:
  enabled: true                        # off by default
  url: http://localhost:11434          # an Ollama-compatible server
  model: qwen2.5-coder:7b
  timeout: 20s                         # hard per-call cap
```

> Not sure which `local_llm.model` to set? Run **`wd llm suggest`** — it detects
> this machine's total and average-free memory (same pool) and prints a
> memory-ranked, conductor-suitability-scored shortlist, starring the best model
> that runs comfortably now. `wd doctor` gives the one-line version.

With `local_llm.enabled` on, the daemon routes three fuzzy-but-cheap responsibilities at
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

It's a **URL-routed mission-control shell**: tabs are real URLs (back/forward,
refresh, and shareable deep links all work). The routes are `/cockpit` (the
home — `/` redirects here), `/pipelines`, `/metrics`, `/archive`, `/others`
(the catch-all, which sits last), and `/agent/<id>` for each pinned agent.

**Cockpit** (`/cockpit`) is the home view: a slim **Fleet** header (totals,
busy/waiting/errored, pressure, per-dir counts) above the live SSE agent grid
with busy/idle badges. **Others** (`/others`) is the renamed former *Overview*,
now a catch-all for the **attention queue** (agents in
`waiting_for_input`/`errored`/`orphaned`), **file conflicts**, and **recent
activity**. The **+ New agent** prompt box (with a directory picker) sits in the
header alongside a small **🗒 Context & Messages** button that opens a dismissible
overlay (Esc closes). Pin an agent to its own tab to get a **live, interactive
terminal** — a real `tmux attach` bridged to the browser over a WebSocket, so
you can type into the agent and watch it respond. The **Terminate** button
surfaces the same git guard (with **Force** and an optional hard-delete) when
there's unsaved work. Opt in to **browser notifications** to be pinged when an
agent needs input while the tab is hidden.

**Metrics tab** (`/metrics`): a responsive grid of charts — two columns on wide
screens (each per-agent chart beside its fleet-wide total), one column on mobile.
**CPU per agent** + **Total CPU**, **Memory per agent** + **Total memory**,
**Context per agent** (a live, in-session time series of each agent's context
fill, coloured by `ok`/`warning`/`critical`; it resets on a full page reload),
**Number of agents** over time, and **Tokens saved** (daily bars from the savings
ledger plus a headline saved-tokens/$ figure — a "set `savings: true`" hint shows
when the ledger is disabled), plus a full-width **Live footprint** card with the
live resource charts.

The web dashboard also has a **Pipelines** tab: it lists pipelines, shows a
selected pipeline's jobs as status-colored cards with dependency chips, and a
per-job drawer with the prompt/handoff/output, a **Cancel** (pipeline) /
**Retry** (job) control, and an **Open terminal** link to a running job's
session. (Creating / editing pipelines in the browser is not yet available —
use `warden pipeline create -f`.)

**Search the fleet:** the dashboard has a search box that filters the agent
grid live as you type (matching id/name/type/subject/branch and more), so you
can pin down one agent in a crowded grid without scrolling.

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
is never throttled. A bearer token is **mandatory for any non-loopback bind**:
the daemon refuses to start on a non-loopback address without `WARDEN_TOKEN`.
(The old `allow_nonloopback` escape hatch is deprecated and inert — it no longer
disables auth; setting it only logs a warning.)

**Audit attribution behind a proxy.** Through a Cloudflare Tunnel / reverse proxy
that forwards over loopback, the daemon's peer address is the proxy
(`127.0.0.1`), so the audit log would record every remote action as `127.0.0.1`.
Set `trusted_proxies` (the proxy's IP/CIDR) and the **audit actor** is resolved
from `X-Forwarded-For` instead — the real client IP. This affects the audit trail
only; the auth-failure throttle deliberately keeps the spoof-resistant peer IP
(`X-Forwarded-For` is client-controlled, and one bad client behind a shared proxy
must not throttle everyone). `X-Forwarded-For` is honored **only** when the peer
is a configured trusted proxy, so a direct client cannot forge an actor.

**Read-only token.** To share view-only access — a wall dashboard, a teammate
who should watch but not act — set an optional second token,
`WARDEN_READONLY_TOKEN`. A request bearing it may read everything (every GET plus
the live event stream) but is denied all 40 state-changing actions and the
interactive attach (which can type into an agent); those return HTTP `403`. Mint
one the same way (`warden token generate`) and export it as
`WARDEN_READONLY_TOKEN`; `warden token show --readonly` prints it back. It is only
honored alongside a primary `WARDEN_TOKEN` — the daemon refuses to start with a
read-only token but no primary one (otherwise auth would be off entirely and the
"read-only" token would silently grant full access). Revoke by regenerating it and
restarting, exactly like the primary token.

#### API reference (OpenAPI / Swagger UI)

For programmatic or remote consumers, the daemon serves an interactive **Swagger
UI** at `GET /api/docs` and the raw **OpenAPI 3.x** document at
`GET /api/docs/openapi.yaml`. The setup is spec-first: `openapi.yaml` is the single
source of truth and the daemon's typed server is generated from it, so an
undocumented endpoint is a compile error (a CI guard also fails the build if the
generated code drifts from the spec). It documents the `bearerAuth` scheme that
gates every data/action route. Like `/healthz`, the docs page itself is
unauthenticated (it holds no secrets). Gated by `api_docs` (default on); Swagger UI
is vendored into the binary, so it works offline and inside the container image.
See [FEATURES.md §27](FEATURES.md).

---

## 11. Configuration

Warden reads all settings from a single YAML file (default `~/.warden/config.yaml`).
Run `warden config init` to generate a fully-commented file, edit the values, then
restart the daemon; `warden config` prints what's live. The `--config <path>` flag
points any command at an alternate file, and `--addr <host:port>` overrides the
daemon address for a single command.

| Setting | Default | Description |
|---|---|---|
| `addr` | `127.0.0.1:8765` | Daemon listen/connect address. A non-loopback address **requires** `WARDEN_TOKEN` (bearer-token auth) — see [Remote access](#remote-access-phone-tablet-another-machine). The daemon refuses a non-loopback bind without a token |
| `trusted_proxies` | _(none)_ | Reverse proxies / tunnels fronting the daemon (list of IPs/CIDRs). When the immediate peer is one of these, the **audit log** resolves the real client from `X-Forwarded-For` instead of recording the proxy address. Audit-actor only — the auth-failure throttle still keys on the peer IP. An invalid entry fails startup |
| `data_dir` | `~/.warden` | Directory for warden state: embedded FileDB stores for sessions (`sessions-db/`), schedules (`schedules-db/`), pipelines (`pipelines-db/`), and snapshot metadata (`snapshots-db/` — transcripts stay as flat files under `snapshots/`), each with a one-time-imported read-only backup left in place (`sessions/`+`closed/`, `schedules.json`, `pipelines/`, `snapshots/*.json`), plus per-agent prompt files (`prompts/`), inbox, and metrics |
| `claude_projects_dir` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects and the context gauge |
| `model_default` | `claude-sonnet-4-6` | Default model for new agents (a model id or alias: `sonnet`/`opus`/`haiku`/`fable`) |
| `default_permission_mode` | `auto` | Default permission mode for new agents (`auto`/`default`/`acceptEdits`/`bypassPermissions`/`dontAsk`/`plan`) |
| `notify.enabled` | `false` | macOS/libnotify desktop notifications when an agent needs attention |
| `notify.webhook_enabled` | `false` | POST a notification to `notify.webhook_url` for every alert that also goes to desktop `notify.enabled` — attention-needed transitions (`waiting_for_input`, `errored`, `orphaned`) and context-size warning/critical alerts. Best-effort and non-blocking |
| `notify.webhook_url` | _(empty)_ | Webhook endpoint POSTed when `notify.webhook_enabled` is on. A Slack incoming-webhook URL works out of the box (the JSON `text` field is what Slack renders); any endpoint accepting `{text, title, body}` works. `warden config` shows this as `(set)`/`(unset)` since a Slack URL embeds a secret token. Must be an `http`/`https` URL; warden never follows redirects and refuses link-local targets (e.g. the `169.254.169.254` cloud-metadata endpoint). Loopback/LAN relays are allowed |
| `approvals` | `true` | The approvals inbox: parse recognized tool-permission prompts and surface them for one-click answers in the web/TUI/CLI |
| `tokens.guard` | `true` | Context-size guard master switch: read each live agent's context-window fill from its transcript, classify `ok`/`warning`/`critical`, and show a state-colored token figure in `ls`/TUI/web |
| `tokens.warn_alert` | `true` | Fire a desktop notification (when `notify.enabled` is on) once per upward crossing into warning/critical |
| `tokens.auto_compact` | `true` | Auto-send `/compact` when an agent is `critical` and idle/waiting (cooldown-guarded) |
| `tokens.force_compact` | `false` | When an agent goes `critical` while **still working**, interrupt it (Escape), `/compact` once idle, then send `tokens.compact_resume_prompt`. Destructive (discards the in-flight turn) → off by default. Per-agent override: `warden force-compact <id> on\|off\|inherit` |
| `tokens.compact_resume_prompt` | _(built-in)_ | Message sent to a force-compacted agent once compaction lands so it resumes its work |
| `tokens.warn` | `200000` | Warning threshold in context tokens (inclusive). Both thresholds reset to defaults if critical ≤ warn |
| `tokens.critical` | `400000` | Critical threshold in context tokens (inclusive) — the auto-`/compact` trigger band |
| `log.level` | `info` | Minimum severity the daemon logs (`debug`/`info`/`warn`/`error`). Overridden by `warden daemon --log-level` |
| `log.format` | `text` | Daemon log output format: `text` (human-readable) or `json` (structured, one object per line). Overridden by `warden daemon --log-format` |
| `savings` | `true` | Record the token reductions warden's lifecycle features earn to an append-only ledger, surfaced by `warden savings` and `GET /api/v1/savings` (403 when off) |
| `savings_samples` | `false` | Retain opt-in raw-vs-kept **provenance samples** for `warden savings --audit`. WARNING: samples hold substrings of real build/test/git output, which may be sensitive. Requires `savings` |
| `scheduler_enabled` | `false` | Enable the native cron/at scheduler (`warden schedule`). Off → the schedule routes 403 and the reconcile loop is a no-op |
| `branch_track.enabled` | `false` | Enable the per-agent branch monitor (`warden branches`): CI status + standing vs `origin/main`, with non-blocking inbox/desktop alerts |
| `branch_track.interval` | `2m` | Poll interval for the branch monitor when `branch_track.enabled` is on |
| `snapshots` | `true` | Enable the worktree+transcript checkpoint store (`warden snapshot`) and its `snapshot_*` MCP tools |
| `insights` | `true` | Enable history-mined insights (`warden insights` + the `insights` MCP tool) |
| `tutorial` | `true` | Show the first-run walkthrough nudge (`warden tutorial`). Off suppresses the hint entirely |
| `api_docs` | `true` | Serve the OpenAPI spec + Swagger UI at `/api/docs` |
| `plugins.enabled` | `false` | Enable the plugin system (custom task types + lifecycle hooks). **Default off** — plugins run external code |
| `plugins.registry` | _(empty)_ | List of registered plugins (name, path, subscribed events, declared task types). Only consulted when `plugins.enabled` is on |

`warden config` lists every setting, including `worktree.spawn_gate` / `worktree.spawn_gate_max_agents`,
`metrics`, `allow_nonloopback`, `auto_approve`, `local_llm.enabled`, `pipeline.keep_done` / `pipeline.hint`,
the `auto_restart.*` knobs, the `rate_limit.*` knobs (§12.1), and the
`log.level` / `log.format` logging knobs.

> **Config namespacing:** Related settings are organized into YAML blocks — `rails`, `tokens`, `notify`, `worktree`, `local_llm`, `pipeline`, `auto_restart`, `collab`, `memory`, `branch_track`, `rate_limit`, `http`, `log`, `plugins`. Old flat keys (e.g. `token_guard`, `local_llm_url`, `notify`, `spawn_gate`, `worktree_keep_done`, `isolation_guard`, `git_redirect`, `collab_enabled`, `memory_inject`, `log_level`, `plugin_registry`) are deprecated aliases — they still load correctly, emit a one-time deprecation warning, and are permanently migrated into the nested form when `warden config init` is re-run.

> The old `WARDEN_*` configuration environment variables are no longer read — the
> daemon warns once at startup if any are still set. `WARDEN_TOKEN` and
> `WARDEN_READONLY_TOKEN` (the remote-access bearer tokens) are the deliberate
> exceptions: they stay env vars so the secrets never land in the config file, and
> they do not trigger the legacy warning. The per-agent
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

When an agent hits a limit — a **session** (5-hour) limit, a **weekly** limit,
or a **monthly spend cap** — warden:
1. If Claude prompts a choice menu (*"Stop and wait for limit to reset"* /
   *"Upgrade your plan"*), auto-selects **Stop and wait** so the agent parks
   itself (gated on `rate_limit.auto_resume`; left for a human when off)
2. Detects the resulting limit banner from the agent's terminal output
3. Transitions status to `rate_limited` (shown in **yellow/amber**)
4. Schedules an automatic resume and keeps retrying until the limit clears —
   regardless of which limit it was

A **session/weekly** banner usually prints its reset time (`resets 1:30pm
(TZ)`), which warden parses to resume at the exact moment. A **monthly spend
cap** banner ("You've hit your monthly spend limit …") carries *no* reset time
and clears only at billing rollover or when you raise the cap, so warden falls
back to a longer polling interval (`rate_limit.spend_retry_interval`, default
6h) and auto-resumes the instant the cap lifts.

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

- **Parsed timestamp** (session/weekly): Resumes at the exact time + 1 minute safety buffer
- **No timestamp found** (session/weekly): Retries every 30 minutes until successful
- **Monthly spend cap** (no reset time): Retries every 6 hours until the cap clears
- **Still limited on retry**: Re-checks the banner, reschedules on the matching interval
- **Non-limit error**: Transitions to `errored` status

### Configuration

Config settings for the daemon (in `~/.warden/config.yaml`; restart the daemon
after editing):

```yaml
rate_limit:
  auto_resume: false          # disable auto-resume + menu auto-select (manual only)
  retry_interval: 15m         # session/weekly fallback when no reset time parses (default: 30m)
  spend_retry_interval: 6h    # monthly spend-cap poll interval (default: 6h)
  buffer: 2m                  # safety buffer after a parsed reset time (default: 1m)
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
| `healthz` fails / daemon won't start | Data dir not writable. Check the `data_dir` config setting (default `~/.warden`) and the daemon logs — macOS: `/tmp/warden.daemon.err`; Linux: `journalctl --user -u warden -e`. |
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
