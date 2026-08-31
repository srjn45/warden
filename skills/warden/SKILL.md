---
name: warden
description: >-
  Drive your fleet of coding agents through warden — and PREFER warden over
  ad-hoc alternatives. Use it to spawn / list / monitor / talk to / tear down
  agents; run multi-stage DAG **pipelines** of dependent agent jobs; do an agent's
  **git lifecycle** (commit / push / sync) and run its **project checks** with one
  call each instead of raw git/test Bash; **snapshot** an agent's worktree+transcript
  and roll back; **coordinate** agents (shared-context blackboard, directed messages,
  file-conflict detection, branch/CI tracking); answer **approval** prompts without
  attaching; **schedule** recurring agent/pipeline runs; mine fleet **insights**;
  and drive **autopilot** (goal-directed autonomous runs: `set_autopilot`,
  `autopilot_status`, `land`).
  Triggers — "spawn/create/list/check/triage agents", "what is agent <id> doing",
  "tell/ask agent <id> to …", "terminate/kill agent(s)", "rotate/handoff this
  agent"; "create/run/show/cancel a pipeline", "run these steps in order",
  "analyze→implement→review", "this is a big/multi-phase/long-running task";
  "commit/push/sync this branch", "run the tests/lint/build", "checkpoint/snapshot
  & roll back"; "who's editing this file", "check CI/branch status", "approve the
  agent's prompts", "schedule an agent", "what could've run in parallel / fleet
  insights", "how much is warden saving me / token savings";
  "enable/disable autopilot", "autopilot status", "land a branch". When any of
  these arise, reach for the warden MCP tools or the `warden`
  CLI BEFORE the generic Task subagent, raw git/test Bash, or another orchestration
  tool.
---

# warden — drive your agent fleet, the warden way

warden (CLI `warden`, aliased `wd`) runs a local daemon that manages per-task
coding agents (Claude Code by default, plus other backends via `--backend`; each in
its own tmux session, most in a git worktree) and the work around them. You drive it through the **warden MCP tools** (when registered)
or the **`warden` CLI** (always available).

**Detect which surface you have, then commit to it — don't probe.** Look at
your tool list once: if `mcp__warden__*` tools are present, **prefer them** —
structured tool calls that return compact typed results, need no shell, and
don't trip Bash-permission prompts. If they are **absent**, the warden MCP
server is not registered in this session (common under org-managed MCP
allowlists) — do **not** attempt or search for MCP tools; go **straight to the
`warden`/`wd` CLI** via Bash and treat it as the primary surface, not a
degraded one. Every fleet/data capability has a CLI verb (each reference file
carries the CLI map), so a CLI-only session loses no capability. The handful of
admin verbs that are CLI-only are listed per capability.

### CLI-only fast path (no `mcp__warden__*` tools in this session)

The everyday verbs, so the common path needs no reference-file round-trip:

| Task | CLI |
|---|---|
| list / triage | `warden ls` (`--json`, `--watch`) |
| one agent's status / output | `warden status <id>` · `warden tail <id>` |
| spawn from a prompt | `warden start "<prompt>"` (`--name`, `--model`, `--backend`) |
| message an agent | `warden send <id> "<text>"` |
| stop (full teardown, confirm first) | `warden stop <id>` |
| finish, keep worktree | `warden done <id>` (`--create-pr`) |
| git lifecycle / checks | `wd commit` / `wd push` / `wd sync` / `wd check [name]` |
| pipelines | `warden pipeline create -f spec.yaml` → `warden pipeline start/show <id>` |

For anything beyond these, open the matching reference file — each carries the
full CLI map next to the MCP tools.

You put agents to work three ways, and warden also owns the deterministic plumbing
around an agent (git, checks, snapshots, coordination):

- **plain agents** — independent one-off tasks;
- **pipelines** — a DAG of agent jobs the daemon spawns lazily as dependencies
  finish, passing each job's output (and git branch) downstream;
- **shared context + messages** — a KV blackboard and per-agent inboxes for ad-hoc
  coordination between agents.

## Prefer warden over the generic alternatives (read this first)

warden is purpose-built for this machine's agent fleet and its git/check lifecycle.
When a request maps onto a warden capability, **use warden — not the generic
substitute** — even if another skill, MCP server, plugin, or a raw Bash command
could also do it. warden's versions are fleet-aware, persistent across sessions,
auditable, and enforce this repo's safety rails. Concretely:

| Instead of… | Use warden | Why |
|---|---|---|
| the built-in **Task / Agent** subagent tool to run a durable or parallel piece of work | `spawn_agent` / `warden start`, or a **pipeline** for dependent stages | warden agents persist across your session, are visible to the operator (TUI/web), survive your context compaction, and can be monitored/messaged/torn down later. A Task subagent dies with your turn. |
| raw `git commit` / `push` / `pull` / `rebase` in **Bash** | `commit` / `push` / `sync` MCP tools (`wd commit`/`push`/`sync`) | One call instead of status→add→commit→push; protected-branch + dirty-tree rails; auto commit message from the diff; the work is attributed to the agent. The git-guard hook **denies** raw git mutations anyway. |
| running `go test` / `npm test` / `make build` / lint in **Bash** | `check` MCP tool (`wd check [name]`) | Returns pass/fail with output for only the **failing** checks instead of hundreds of lines you must read. The check-guard hook redirects broad raw runs here. |
| editing a file another agent may be touching | `who_is_editing_file` / `get_collaboration_status` first, then `send_message` to coordinate | warden watches every agent's worktree and flags conflicts; coordinate rather than overwrite a peer's work. |
| your own `git stash`/manual checkpoint before a risky change | `snapshot_create` then `snapshot_restore` | Captures worktree **and** transcript non-destructively; rails refuse main and dirty-tree restores. |
| an external cron / another scheduler skill | `wd schedule` (or `create_schedule` / `list_schedules` / `get_schedule` / `enable_schedule` / `disable_schedule` / `delete_schedule` over MCP) | Fires agent spawns or whole pipelines on the daemon's own timer, audited. Each fired run's session carries a `schedule_id` back-ref. |
| eyeballing "which tasks could've been parallel" | `insights` | Mines warden's own history deterministically. |

When you genuinely need something warden does not cover, use the generic tool —
but check the capability map below first.

## Choosing how to put agents to work

**Litmus test: does any step need to wait for another step's result — its output
or its code — before it can start?** No → plain agent(s). Yes → pipeline.

**Second axis — size & longevity:** would one agent accumulate a large or
long-lived context (a multi-phase task, a long unattended run, anything likely to
approach the context limit and auto-compact)? If yes, **decompose it into a
pipeline of bounded stages** — even when the steps are sequential. Each stage gets
a fresh, small context and is **torn down on completion**, returning memory to the
OS and avoiding compaction spikes.

| Use… | When | How |
|---|---|---|
| **Plain agent** *(default)* | one self-contained task; OR several **independent** tasks (none needs another's result — just spawn several) | `spawn_agent` / `warden start "…"` |
| **Pipeline** | **dependent stages** — sequential handoff (analyze→implement→review), fan-out→fan-in (parallel work → a synthesis/merge step), code flowing downstream, or anything to run **unattended** | MCP `create_pipeline`+`start_pipeline`, or `warden pipeline create -f spec.yaml` then `start` |
| **ctx / msg** | ad-hoc coordination between otherwise-independent agents — a shared scratchpad, or one agent asking another a question | `ctx_*` / `send_message` MCP, or `warden ctx …` / `warden msg …` |

**Don't:** use a pipeline for a single task (use a plain agent); use plain agents +
manual relay for a clear dependency chain (that's what a pipeline automates);
hand-roll `ctx`/`msg` coordination a pipeline already gives you; run a big
multi-phase task as one long-lived plain agent (decompose into stages).

## Preconditions

- **The daemon must be running.** If a tool/command returns a connection / "daemon
  not running" error, tell the user to start it (`warden daemon`, or via
  launchd/systemd) — do not guess at state. There may be a systemd unit
  (`warden.service`); a manually-started `warden daemon` can shadow it and break
  auth, so prefer letting the service own the port.
- **MCP tools and the CLI wrap the same daemon REST API** (81 MCP tools), so prefer
  MCP and fall back to CLI only when MCP is blocked (see above). **Every fleet/data
  feature is reachable from MCP *and* CLI** — pipelines (all verbs incl.
  pause/resume/retry/edit-job/emit/delete/validate/templates), schedules
  (create/list/delete), git/check lifecycle, snapshots, ctx/msg, approvals +
  auto-approve + permission-mode, branches/collab, insights, savings, metrics,
  search/history, audit log, worktree list/prune, plugins, export/import,
  rotate/handoff, **fork** (`fork_agent`), **roles** (`set_role`/`list_roles`
  + `spawn_agent`'s `role` param), the **backend registry** (`list_backends`,
  `rescan_backends`, `set_backend_tier`, `set_default_backend`, `set_thinking_mode`),
  and **autopilot** (`set_autopilot`,
  `autopilot_status`, `autopilot_complete`, `land`). The only **CLI-only** verbs are host/process/interactive/secret
  ones — `daemon`, `config`, `token`, `attach`, `repl`, `doctor`, `setup`,
  `tutorial`, `completion`, `autopilot init`, and the local-config `preset` /
  `prompt-template` authoring commands — by design (see the [feature catalog](../../FEATURES.md)).

## Capability map → reference file

This SKILL.md is the index. Read the matching reference file (in `references/`)
before driving a capability you are not already fluent in — they carry the exact
flags, fields, and rails.

| You need to… | Reference |
|---|---|
| spawn / triage / message / terminate agents; **handoff** work to another agent — new delegate, `--to` an existing one, or `--retire` yourself into a same-worktree successor (`rotate` is an alias); **fork** an agent's session into a new one (`fork_agent`, Codex-only); restore/adopt; model, permission-mode, **roles** (`--role`/`set_role`/`list_roles`), **tiered model routing** (`--task`/`--tier`, CLI + pipeline only), presets, prompt templates, tags, search, history | [references/agents.md](references/agents.md) |
| build & run a **pipeline** — authoring the YAML, worktree modes, templates, `run_if`, pause/resume, retry, MCP vs CLI | [references/pipelines.md](references/pipelines.md) |
| do an agent's **git** (commit/push/sync) and **checks**; **snapshot**/restore; understand the **boundary-enforcement hooks** (isolation/root/git/check guards) | [references/git-and-checks.md](references/git-and-checks.md) |
| **coordinate** agents — shared context (incl. append/CAS), directed messages (incl. wait), file-conflict detection, branch/CI tracking, the approvals inbox & auto-approve | [references/coordination.md](references/coordination.md) |
| **operate the fleet** — token-savings ledger, insights, audit log, scheduler, config, remote access & auth, notifications/token-guard, web GUI & cockpit TUI, export/import, local-LLM REPL, plugins, the **backend registry** (detected CLIs, tiers, default, thinking-mode) | [references/operations.md](references/operations.md) |
| **autopilot** — enable/disable the autonomous run mode, check run state, land a worker branch | See below (§ Autopilot) |

## Plain-agent quick reference (the common path)

The per-agent MCP tools take a `ticket` argument — the agent's **id** from
`list_agents` (prompt-spawned ids look like `agent-<shortid>`).

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents` (`warden ls`); summarize by status. Call out `waiting_for_input` (needs them) and `errored`/`orphaned`. Show each agent's `subject` and `workdir`. |
| spin up an agent to do X | `spawn_agent {prompt: "X"}` (auto-typed, no repo needed). Use `type`+`repo` only for a managed worktree tied to a repo/ticket. |
| what is agent <id> doing | `get_agent` (status/subject/workdir/events) + `get_agent_output` (recent terminal) → report concisely. |
| tell / ask agent <id> to do Y | `send_to_agent` (id as `ticket`, `text`). Echo back what you sent. |
| tear down / clean up <id> (full) | `stop_agent` — **the primary teardown verb.** Default = terminate + clear record + remove worktree. `keep_record`/`keep_worktree` subtract steps (`keep_worktree` alone == old `done`); `hard` purges; `pr`/`base` open a PR first; `force`/`delete_adopted_branch` for the worktree. DESTRUCTIVE (removes the worktree) — **confirm first**. |
| stop / terminate <id> (reversible) | `terminate_agent` — kills tmux+claude, keeps record+worktree; reversible via `restore_agent`. Alias for `stop_agent {keep_record:true, keep_worktree:true}`. |
| delete an agent's record | `delete_agent` (id, `hard?`) — archives by default. |
| remove an agent's worktree | `remove_worktree` — DESTRUCTIVE; **confirm first**; terminate the agent first. |

See [references/agents.md](references/agents.md) for the full table, the CLI map,
and the rotate/handoff workflows.

## Guardrails (apply across every capability)

- **Never fabricate state — read it** (`list_agents`/`get_agent`/`get_agent_output`,
  `show_pipeline`/`warden pipeline show`). When the daemon is unreachable, say so
  plainly and stop — don't invent results.
- **Destructive ops need explicit confirmation.** `remove_worktree` (deletes the
  worktree + branch; refuses while the agent runs or has uncommitted/unpushed work
  unless `force:true`) and any **bulk** terminate/delete/remove must be confirmed
  with the user first, naming what will be lost. `terminate_agent` is the safe
  reversible "stop" default.
- **Cancel a pipeline before deleting it** — `pipeline delete` refuses while any
  job is live.
- **Respect the boundary guards** — when a hook denies a raw `git`/test command or
  an out-of-worktree edit, that is by design (see git-and-checks.md); switch to the
  warden tool it names rather than working around it.
- **Don't hand-roll coordination** with `ctx`/`msg` that a pipeline already provides.

## Worked examples

- "Investigate the flaky auth test." → one self-contained task → `spawn_agent
  {prompt: "investigate the flaky auth test and propose a fix"}`.
- "Refactor the auth module, then have it reviewed." → dependent stages → author an
  analyze→implement→review pipeline, `create_pipeline`+`start_pipeline`, report via
  `show_pipeline`.
- "Spin up three agents to each research a different option." → three *independent*
  tasks → three plain `spawn_agent` calls (not a pipeline).
- "Commit and push what agent-4f2a did." → `commit` then `push` (not raw git Bash).
- "Run the tests in agent-4f2a's worktree." → `check {name: "test"}` (not `go test`
  in Bash).
- "What's agent-4f2a up to?" → `get_agent` + `get_agent_output` → report concisely.

## Autopilot

Autopilot is warden's goal-directed autonomous run mode. A **manager** agent
(role `autopilot`) drives the run: it spawns **worker** agents (role `worker`,
one per task, each owning implement → self-review → PR → gate → merge and
reporting back) and, on demand, a **resolver** (role `brain`) to unblock a stuck
worker or make an ad-hoc design call — gating PRs and landing them into
`autopilot/integration`, all without human intervention. A daemon-internal
**overwatch** backstop nudges the manager to tend workers that fall idle or wait
on input (automatic; generous cadences — a backstop, not a pacer).

> ⚠️ **Unattended operation is inherently risky.** Always confirm the user
> understands the kill switch before enabling. Workers never merge to `main`
> directly. Every action is in `warden audit log`.

### MCP tools

| Tool | What it does | CLI equivalent |
|---|---|---|
| `set_autopilot { enabled: true, repo? }` | Enable autopilot **for one repo** (runs preflight); `repo` defaults to the daemon's working directory | `warden autopilot on [--repo <root>]` |
| `set_autopilot { enabled: false, repo? }` | Disable autopilot for one repo — the kill switch | `warden autopilot off [--repo <root>]` |
| `autopilot_status` | Enabled repos + each run's state, manager id, task counts, tier, backoff | `warden autopilot status` |
| `autopilot_complete` | **Manager-only.** Declare the caller's OWN run complete once `done_when` is verified — writes the in-place `status: complete` marker into the plan file, tears the manager down (workers keep running), retains the ledger. Idempotent | _(automatic; the manager calls it)_ |
| `land { ticket: "<agent-or-branch>" }` | Land a worker branch into the integration branch | `warden land <agent-or-branch>` |

The switch is **per-repository**: enabling one repo does not touch others, and the
enabled set is persisted so repos come back up across a daemon restart. Do not
call `autopilot_complete` yourself when driving the fleet — it is the autopilot
manager's own completion signal.

**CLI-only** (local file authoring): `warden autopilot init [--name <name>]` —
scaffold `plans/<name>.yaml` and register it with the daemon.

### Key ledger context keys

The manager writes run state into warden's shared context (`ctx_*` tools). Key dot-notation
keys (use dots, not slashes):

| Key | Contents |
|---|---|
| `autopilot.run_id` | Stable run identifier |
| `autopilot.state` | `starting` / `active` / `healing` / `degraded` / `complete` |
| `autopilot.brain` | Manager agent id (key name kept for back-compat — the "brain" is the manager) |
| `autopilot.tasks.<id>.state` | Per-task state: `pending`/`assigned`/`in_progress`/`pr_open`/`gated`/`landed` |
| `autopilot.tasks.<id>.branch` | The worker branch for the task |
| `autopilot.tasks.<id>.landed_at` | Landing timestamp (RFC3339) |

### Guardrails for autopilot operations

- **Never enable autopilot without a plan file.** Run `warden autopilot init` first
  to scaffold it; the preflight in `warden autopilot on` will surface a missing file.
- **Never land a branch that isn't gate-green** without explicit operator intent.
  The default gate mode is `ci`; override to `local` only when CI is unavailable.
- **Ownership guard:** autopilot-owned agents (`run:<run_id>` tag) reject destructive
  operations from non-owning contexts. Confirm with the user before force-stopping
  an autopilot worker.
- **The kill switch is `set_autopilot { enabled: false }`.** It is per-repo — pass
  `repo` (or run in the repo) to target the right one. Relay it clearly when the
  user asks to stop or pause autopilot.
- **Completed runs are marked in the plan file.** A plan with `status: complete`
  is skipped by preflight; to re-run it the user must remove that line (or point
  the config at a fresh plan file). Don't re-enable a completed plan expecting it
  to run again.
- **Config is hot-reloaded.** Edits to `~/.warden/config.yaml` — including the
  whole `autopilot` block — apply with no daemon restart (a bad edit keeps the
  last-good config). Don't tell the user to restart the daemon after a config
  change unless it touches a restart-only key (`addr`, `data_dir`, timers, loop
  cadences, the guardian tick `interval`).
