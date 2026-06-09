# agentctl — Design

**Date:** 2026-06-01
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Location:** `/Users/srajan.pathak/workspace/personal/agentctl`

> A personal tool. Independent of work / the-monorepo. It *operates on* git
> worktrees and tmux sessions in any repo you point it at, but the tool itself
> lives in personal space.

---

## 1. Problem

I run multiple Claude Code agents, each in its own tmux session. Many are tied
to a Jira ticket in a dedicated git worktree (`.worktrees/<TICKET>`), but agents
also do work with no ticket at all — a research spike, reviewing someone's PR,
debugging a Buildkite failure, running tests. Today this is unmanaged, which
hurts in four ways:

1. **Knowing status** — I can't tell at a glance which agents are working, idle,
   waiting for input, done, or errored without attaching to each session.
2. **Switching / attaching** — tedious to remember names and jump between
   sessions to find the one that needs attention.
3. **History / audit** — no persistent record of what each agent did, when it
   started/finished, or which ticket it maps to.
4. **Cleanup** — manually killing tmux sessions and pruning worktrees/branches is
   error-prone and leaves drift (e.g. stale worktrees with no live session).

## 2. Goals

- A **CLI-first** manager (`agents …`) to spawn, observe, attach to, and clean up
  agent sessions of different **task types** (development, analysis/spike,
  pr-review, buildkite-debug, test-run, env-test, …).
- **Continuous monitoring** of every session's status, not just on-demand.
- A **persistent, schema-flexible** record of every session.
- One source of truth that a **future web GUI** can reuse without a rewrite.
- An **MCP orchestrator interface**: a common Claude console that, via MCP, can
  list/spawn/clean up agents and **talk to a specific running agent** (send input,
  read its output) and report back to me.
- I drive it through Claude prompts today; the CLI + MCP are the contract underneath.

### Task types

Every session has a `type`. The type drives whether a git worktree is created:

| Type | Worktree behavior |
|---|---|
| `development` | New worktree + **new branch** (branch = id, or `--branch`). |
| `pr-review` | Worktree checked out on an **existing** PR branch (`--pr` / `--branch`). |
| `analysis` / `spike` | **Optional** scratch worktree via `--worktree` (default: none). |
| `buildkite-debug` | No worktree — bare agent in the repo. |
| `test-run` (unit/integration) | No worktree. |
| `env-test` | No worktree. |
| `other` (catch-all / unknown) | No worktree. |

All new **development** is always done in a worktree via these managed agents —
never directly in the main checkout. Parallelism is the working model agentctl
enables (run many isolated worktree+agent sessions at once); agentctl itself adds
no fan-out machinery — I, or the orchestrator Claude via MCP, decide when to spawn
several.

## 3. Non-Goals (YAGNI for v1)

- Web GUI (designed for, not built in v1).
- Push notifications (Slack / macOS) — deferred; the daemon makes them easy later.
- Managing non–Claude-Code agents (cursor-agent, etc.). v1 assumes **Claude Code**.
- Multi-machine / remote sessions. Localhost only.
- agentctl-driven task fan-out / parent-child sub-session grouping — parallelism is
  the working model, not a feature with its own machinery.

## 4. Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Architecture | **Local daemon + Mongo + thin CLI** (Approach A) | Continuous monitoring + future web GUI reuse the same hub; single writer to Mongo. |
| Language | **Go** | Best fit for a CLI + always-on poller daemon: single static binary (CLI and daemon in one), goroutine concurrency for polling, no runtime/venv to manage, easy to copy anywhere. |
| Status source | **Hooks (primary) + pane-scrape (fallback)** | Claude Code hooks give precise event-driven status; polling catches hangs/crashes/drift. |
| Data store | **MongoDB (local)** | Schema-flexible (add fields without migration); one document per session. |
| Lifecycle role | **Spawn + manage** | Daemon creates worktree+tmux+claude, so cleanup is exact. |
| Agent CLI | **Claude Code, `--dangerously-skip-permissions`** | Hooks are first-class. Every spawned session skips permission prompts so agents run unattended; the `Notification` hook still records when one *would* have prompted. |
| Session id | **Ticket if provided, else `<type>-<shortid>`** | Sessions may have no Jira ticket; `ticket` is an optional field, id/tmux-name is always present. |
| Worktree creation | **Per task type** (see §2) | development always isolated; read-only tasks (buildkite-debug, test-run) need no branch. |
| Orchestration | **MCP stdio server (`agentctl mcp`)** | Lets a common orchestrator Claude list/spawn/clean up agents and send input / read output of a specific agent — a thin bridge over the same daemon REST API (no direct Mongo). |
| Cleanup | **Kill tmux + archive doc; remove worktree/branch only if one exists, GUARDED** | Abort on uncommitted/unpushed changes; `--force` to override. No-worktree sessions skip the git guard/prune. |

## 5. Architecture

```
agentctl/                         /Users/srajan.pathak/workspace/personal/agentctl
├── cmd/agentctl/main.go   single binary entrypoint; Cobra root command
├── internal/
│   ├── daemon/        HTTP server (chi/net-http) — the hub (always-on via launchd)
│   │   ├── server.go      wires store + lifecycle + poller, starts the listener
│   │   └── api.go         REST: /sessions, /sessions/{id}, /events, /spawn, /cleanup,
│   │                            /sessions/{id}/input, /sessions/{id}/output
│   ├── store/         Mongo access (official mongo-go-driver) — the ONLY writer
│   ├── lifecycle/     spawn (per-type worktree?+tmux+claude) and cleanup (kill+guarded prune)
│   ├── poller/        goroutine: tmux pane-scrape every N s, drift detection
│   ├── client/        HTTP client to the daemon (shared by cli + mcp)
│   ├── cli/           Cobra subcommands — thin HTTP client to the daemon
│   └── mcp/           MCP stdio server — tools bridged to the daemon REST API
├── hooks/             scripts wired into ~/.claude/settings.json that curl the daemon
├── docs/specs/        this document
└── go.mod
```

One static binary, four faces:
- `agentctl daemon` — owns Mongo, exposes REST, runs the poller goroutine. The single writer.
- `agentctl ls|start|status|attach|done|send|tail` — thin HTTP client subcommands; no direct Mongo access.
- `agentctl mcp` — stdio MCP server so an orchestrator Claude can manage agents and talk to a specific one.
- **hooks** — one-line `curl` calls from each Claude session; language-agnostic.
- **web GUI (later)** — added routes + a static frontend served by the *same* daemon.

### Data model — one document per session

```json
{
  "_id": "PROJ-350",
  "type": "development",
  "ticket": "PROJ-350",
  "tmux_session": "PROJ-350",
  "repo": "/Users/srajan.pathak/workspace/the-monorepo",
  "worktree": ".worktrees/PROJ-350",
  "branch": "PROJ-350",
  "pr": "",
  "status": "waiting_for_input",
  "pid": 12345,
  "created_at": "2026-06-01T15:40:00Z",
  "updated_at": "2026-06-01T15:42:10Z",
  "events": [
    {"ts": "2026-06-01T15:40:01Z", "type": "SessionStart", "detail": ""},
    {"ts": "2026-06-01T15:42:10Z", "type": "Notification", "detail": "Allow Bash(git push)?"}
  ],
  "last_pane_excerpt": "..."
}
```

A no-ticket session looks the same with an auto id and empty git fields, e.g.:
```json
{ "_id": "buildkitedebug-a1b2", "type": "buildkite-debug", "ticket": "",
  "tmux_session": "buildkitedebug-a1b2", "repo": "/…/the-monorepo",
  "worktree": "", "branch": "", "status": "working", … }
```

**Type enum:** `development | analysis | spike | pr-review | buildkite-debug | test-run | env-test | other`
(unknown types are accepted and treated as `other` → no worktree).

**Status enum:** `spawning | working | waiting_for_input | idle | done | errored | orphaned`

- `_id` = the Jira ticket when one is supplied at spawn, otherwise an auto-generated
  `<type>-<shortid>` (e.g. `spike-a1b2`). `ticket`, `worktree`, `branch`, and `pr` are
  optional and empty when not applicable.

- `orphaned` = doc says active but tmux session is gone (drift).
- Closed sessions move to a `closed` collection on cleanup (archive, not silent loss),
  unless hard-deleted by choice.

Schema-flexible by design: future fields (PR link, token usage, full transcript)
need no migration.

## 6. How it works (end-to-end)

> Command name: the binary is `agentctl`; subcommands below. Alias it to `agents`
> in your shell if you prefer the shorter name.

### Spawn — `agentctl start --type <T> [TICKET] [--repo PATH] [flags]`
The command is type-aware:
```
agentctl start --type development PROJ-350 [--repo PATH] [--branch NAME]
agentctl start --type pr-review --pr 12345 [--repo PATH]      # id auto: prreview-xxxx
agentctl start --type spike --worktree [--repo PATH]          # id auto: spike-xxxx
agentctl start --type buildkite-debug [--repo PATH]           # no worktree
```
CLI → `POST /spawn` → `lifecycle`:
1. Resolve id: ticket if given, else `<type>-<shortid>`.
2. **Worktree (per type, see §2):**
   - `development` → `git worktree add .worktrees/<id> -b <branch>` (adopt if it exists).
   - `pr-review` → fetch the PR and `git worktree add .worktrees/<id> <pr-branch>`.
   - `analysis`/`spike` → only if `--worktree`.
   - `buildkite-debug` / `test-run` / `env-test` / `other` → skip; agent runs in `--repo`.
3. `tmux new-session -d -s <id> -c <worktree-or-repo>`
4. launch `claude --dangerously-skip-permissions` in that session.
5. insert Mongo doc with `type`, optional `ticket`/`worktree`/`branch`/`pr`, `status: spawning`.

Because the daemon created it, teardown later is exact.

### Live status via hooks (precise signal)
Registered once in `~/.claude/settings.json`. Each session identifies itself by its
tmux name (from `$TMUX` / env). On each lifecycle event Claude runs a one-line `curl`:

| Claude hook | Recorded status |
|---|---|
| `SessionStart` | `working` |
| `Notification` | `waiting_for_input` (needs permission / a prompt) |
| `Stop` | `idle` (turn finished) |
| `SubagentStop` | (event-log entry) |
| nonzero exit / crash | `errored` |

Each hook POSTs `{session, type, detail}` → `POST /events` → daemon updates the doc
and appends to `events[]`.

### Background monitoring (safety net) — `internal/poller`
Every N seconds the daemon scrapes each known session with
`tmux capture-pane -p -t <name>` and applies heuristics for what hooks can't see.
The poller persists the pane excerpt only when it *changes*, so `updated_at`
tracks real activity and doubles as a staleness clock:
- `"esc to interrupt"` visible → still `working`.
- a prompt box (`❯` / `Do you want`) visible → `waiting_for_input`.
- a `working` session with no pane change (and no `esc to interrupt`) for ≥ X
  (`stuckAfter`, default 5m) → `idle` (stuck or quietly finished; surfaces for
  attention instead of masquerading as working).
- tmux session gone but doc active → `orphaned`.
- worktrees with no live session (e.g. stale 320/321/322) → flagged for cleanup.

### Query — `agentctl ls` / `agentctl status`, or prompting Claude
All read `GET /sessions`. CLI, my prompts, and the future GUI see the same
Mongo-backed truth. Example glance:
```
PROJ-350   waiting_for_input   2m   "Allow Bash(git push)?"
PROJ-343   working             14m
PROJ-322   orphaned            —    (worktree, no session)
```

### Attach — `agentctl attach PROJ-350`
Convenience wrapper around `tmux attach -t PROJ-350`.

### Talk to an agent — `agentctl send <id> <msg>` / `agentctl tail <id>`
- `send` → `POST /sessions/{id}/input` → `tmux send-keys -t <id> -- "<msg>" Enter`.
- `tail` → `GET /sessions/{id}/output` → `tmux capture-pane -p -t <id>` (last N lines).

### Orchestrator console — `agentctl mcp`
A stdio MCP server registered in a common Claude session's MCP config. It exposes
tools (`list_agents`, `get_agent`, `spawn_agent`, `send_to_agent`,
`get_agent_output`, `cleanup_agent`) that are thin calls to the daemon REST API —
no direct Mongo access, preserving the single-writer model. This lets me ask one
Claude console "what is PROJ-350 doing?" or "tell PROJ-343 to run the
tests", and it identifies the session, talks to that specific agent, and reports back.

### Cleanup — `agentctl done PROJ-350 [--force] [--hard]`
CLI → `POST /cleanup`:
1. **If the session has a worktree** — **Guard:** uncommitted *or* unpushed → **abort
   and touch nothing** unless `--force` (tmux session, worktree, and doc all stay
   intact so you can push and retry — no orphaned-worktree drift). No-worktree
   sessions have no guard.
2. Once past the guard: `tmux kill-session -t <id>`.
3. (worktree sessions only) `git worktree remove` + delete branch.
4. archive the Mongo doc to `closed` (default) or hard-delete with `--hard`.

### Web GUI (later, no rewrite)
Add read routes + a static frontend on the same daemon, polling `GET /sessions`
(or SSE/WebSocket for live updates). Nothing else changes.

## 7. Components to build

1. Go module scaffold (`go.mod`, `cmd/agentctl`, Cobra root); local Mongo via Docker.
2. `internal/store` — Mongo schema (incl. `type`, optional `ticket`/`pr`) + accessors via mongo-go-driver (the only writer).
3. `internal/daemon` — HTTP server (chi/net-http) + REST endpoints (incl. `/sessions/{id}/input` and `/output`).
4. `internal/lifecycle` — per-type spawn (`os/exec` to git / tmux / `claude --dangerously-skip-permissions`) + guarded cleanup + input/output, behind a mockable runner interface.
5. `internal/poller` — background goroutine: pane-scrape + drift detection.
6. `hooks/` scripts + a one-time `settings.json` wiring step.
7. `internal/client` + `internal/cli` — `start / ls / status / attach / done / send / tail` Cobra subcommands (thin HTTP client).
8. `internal/mcp` + `agentctl mcp` — stdio MCP server bridging tools to the daemon REST API.
9. `launchd` plist running `agentctl daemon` so it auto-starts and stays up.
10. Tests (TDD) + README. **Deep review happens after implementation.**

## 8. Error handling & edge cases

- **Daemon down:** CLI prints a clear "daemon not running — start it with
  `agentctl daemon` (or via launchd)" message rather than a stack trace. Hooks fail
  soft (never block the agent if the curl fails).
- **Mongo down:** daemon surfaces a health error; CLI reports it. Hooks fail soft.
- **Duplicate spawn:** `start` on an existing ticket is rejected with a pointer to
  `attach`.
- **Worktree already exists / branch exists:** spawn **adopts** the existing
  worktree+branch and creates a fresh tmux+claude+doc.
- **No-worktree session types:** spawn skips all git steps; cleanup skips the guard
  and prune, only killing tmux + archiving the doc.
- **`--dangerously-skip-permissions`:** every spawned `claude` runs unattended; the
  `Notification` hook still records when a session *would* have prompted, so status
  stays meaningful.
- **Cleanup guard:** (worktree sessions) uncommitted *or* unpushed → abort without
  `--force`, leaving the tmux session, worktree, and doc untouched so the work can be
  pushed and `done` retried (no orphaned-worktree drift). Once past the guard — or for
  no-worktree / `--force` / `--hard` cleanups — tmux is killed and the doc archived so
  state stays consistent.
- **Concurrent hook writes:** routed through the single daemon writer → no races.

## 9. Testing strategy (TDD)

- `internal/store`: tests against a real Mongo via testcontainers-go (no mature
  in-memory Mongo fake for Go).
- `internal/lifecycle`: inject a fake command-runner interface; assert exact
  `git`/`tmux` argv and the cleanup guard logic (the destructive path).
- `internal/poller`: feed canned `capture-pane` output; assert status transitions
  and drift detection.
- `internal/daemon`: `net/http/httptest` over the routes (incl. spawn/cleanup/input/output).
- `internal/client` + `internal/cli`: invoke against a stubbed daemon (httptest server).
- `internal/mcp`: in-memory MCP transport round-trip against a stubbed daemon.

## 10. Resolved decisions (settled during planning)

- **Daemon port + auth:** bind `127.0.0.1:8765`, **no auth** (loopback only); `--addr` to override.
- **Poller interval N / stuck threshold X:** `N = 10s`, `X = 5m`; both flag-configurable.
- **Hard-delete vs archive on cleanup:** **archive** to `closed` by default; `--hard` to delete.
- **Spawned `claude` context:** launched **bare** (`claude --dangerously-skip-permissions`)
  in its worktree/repo. Orchestration happens through MCP `send_to_agent` /
  `get_agent_output`, not a forced first prompt.
- **Session id when no ticket:** auto-generate `<type>-<shortid>`; `ticket` optional.
- **Worktree per type:** see §2 table.
- **Mongo provisioning:** Docker (compose for runtime, testcontainers-go for tests).
