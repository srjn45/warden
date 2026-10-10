---
title: Feature catalog
description: Every warden capability and where you can drive it — CLI, MCP, the /warden skill, the web GUI, and the TUI — with a per-feature coverage matrix.
---

This is the published mirror of [`FEATURES.md`](https://github.com/srjn45/warden/blob/main/FEATURES.md)
in the repository — the authoritative inventory of **every** warden capability and
which surface can drive it.

warden exposes its features across five surfaces — the **CLI** (`warden`, aliased
`wd`), **MCP** (83 structured tools for an orchestrating agent), the **/warden
skill**, the **web** GUI, and the **TUI** cockpit.

**Coverage legend:** ✓ supported · — not applicable / not present on that surface ·
**CLI-only** features are marked and explained (host/process/interactive/secret
operations that are meaningless or unsafe over MCP/web).

## 1. Agent lifecycle

Projects store complete `agents[]`, `pipelines[]`, `terminals[]`, `plans[]`, and
`autopilots[]` membership lists. Plan-run execution entities carry a `plan_id`
back-ref. Agents store `parent_id`, `child_agents[]`, and `child_pipelines[]`;
pipelines store `parent_agent_id`. Job agents belong to pipeline jobs, never
the owning agent’s `child_agents[]`. New projects open empty; reopening restores
hibernated members without auto-spawning an orchestrator. Agent UX states are
`pending`, `busy`, `idle`, `need-input`, `done`, `orphaned`, and `rate_limited`.

Spawn, inspect, message, and tear down per-task coding agents (Claude Code by
default; each in its own tmux session, most in a git worktree).

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Spawn an agent (ticket / prompt / typed) | `start` | `spawn_agent` | ✓ | ✓ | `n` | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| Prompt-spawn (no repo, auto-typed) | `start "<prompt>"` | `spawn_agent` | ✓ | ✓ | `n` | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| List agents | `ls` | `list_agents` | ✓ | ✓ | list | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Agent status / detail | `status` | `get_agent` | ✓ | ✓ | `i` | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Tail recent output | `tail` | `get_agent_output` | ✓ | ✓ | view | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| Send text into an agent | `send` | `send_to_agent` | ✓ | ✓ | — | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| Digest (catch-up summary) | `digest` | `digest` | ✓ | ✓ | `i` | [rotation-digests](https://srjn45.github.io/warden/guides/rotation-digests/) |
| Attach to the live session | `attach` | **CLI-only** (interactive tmux) | ✓ | ✓ (terminal) | `enter` | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| Adopt an existing Claude session | `adopt` | `adopt_agent` | ✓ | — | — | [agents-lifecycle](https://srjn45.github.io/warden/concepts/agents-lifecycle/) |
| **Tear down (umbrella)** | `stop` | `stop_agent` | ✓ | ✓ | `x` | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Finish cleanly (commit/push guard) | `done` (= `stop --keep-worktree`) | `terminate_agent` (`force`) | ✓ | ✓ | `x` | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Terminate | `terminate` (= `stop --keep-record --keep-worktree`) | `terminate_agent` | ✓ | ✓ | `x` | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Restore an orphaned agent | `restore` | `restore_agent` | ✓ | ✓ | `r` | [agents-lifecycle](https://srjn45.github.io/warden/concepts/agents-lifecycle/) |
| Recover an archived `orphaned` agent with a live pane (tombstone-reaper safety net) | `recover` | `recover_agents` | ✓ | — | — | [agents-lifecycle](https://srjn45.github.io/warden/concepts/agents-lifecycle/) |
| Delete / hard-purge | `delete` (= `stop --keep-worktree`, record only) | `delete_agent` | ✓ | ✓ | `D` | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Mandatory agent naming (auto-resolve when `--name`/`name` omitted; role conventions + Fast-Brain-engine slug + adjective-noun fallback; auto-disambiguate) | `start` (omit `--name`) | `spawn_agent` (omit `name`) | ✓ | ✓ | list name col | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| Live 3–5 word activity badge per agent (Fast-Brain; `activity.interval`, default `15s`; keeps previous on failure) | automatic | — (REST `activity` field only) | — | — | agent row badge | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Fast-Brain PR title/body (`KindPRSummary`; per-field fallback; explicit title/body wins) | `agent stop <AGENT> --pr` | — | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Fast-Brain tier router (opt-in `router.use_fast_brain`, default off; lowest precedence, confidence ≥ 0.8) | `start` (omit `--tier`/`--task`/`--role`) | `spawn_agent` (omit pins) | ✓ | — | — | [spawn-and-watch](https://srjn45.github.io/warden/guides/spawn-and-watch/) |
| Rename an agent | `adopt --name` / spawn `name` | `spawn_agent` (`name`) | ✓ | ✓ | info panel | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Tags (group / filter) | `start --tag`, `ls --tag` | `spawn_agent` (`tags`) | ✓ | ✓ | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Model selection | `start --model` / config | `spawn_agent` (`model`) | ✓ | ✓ | — | [env-vars](https://srjn45.github.io/warden/reference/env-vars/) |
| Agent role (built-in persona + default flags) at spawn | `start --role` | `spawn_agent` (`role`) | ✓ | ✓ (Role select) | `ctrl+r` (new-agent) | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/) |
| Switch a running agent's role (relaunch re-injects) | `set-role` | `set_role` | ✓ | — | — | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/) |
| List the built-in role catalog | `role list` | `list_roles` | ✓ | ✓ (Role select) | list | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/) |
| Tiered model routing — derive the model tier from a **task** | `start --task` (CLI + REST; no pipeline `task:`) | routes by `role` only (no `task` param) | — | — | — | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/#roles-tasks-and-tiers) |
| Tiered model routing — pin the model **tier** for the quota-balanced resolver | `start --tier` (also pipeline `tier:`) | routes by `role` only (no `tier` param) | ✓ | — | `ctrl+t` (new-agent; live candidate table) | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/#roles-tasks-and-tiers) |
| List the backend's live model menu | `models` (`--backend`, `--json`) | **CLI-only** (agent-native; local worktree exec, no daemon round-trip) | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |
| Backend selection (Claude / Aider / OpenCode / Codex / Crush / Goose / Cursor / Antigravity) | `start --aicli` | `spawn_agent` (`backend`) | ✓ | — | — (CLI/MCP pin; TUI is tier-first) | [agent-backends](https://srjn45.github.io/warden/concepts/agent-backends/) — only `claude` is stable; `codex` and `antigravity` are β beta; `aider`, `opencode`, `crush`, `goose`, and `cursor` are 🧪 experimental |
| Terminal session (`kind=terminal`) — managed `$SHELL` seat, no AI (prompt ignored); back-compat `backend=terminal` alias | `start --kind terminal` | `spawn_agent` (`kind`) | ✓ | ✓ (Terminals tab) | `t` (in the cursor's project) | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Handoff — delegate (new / `--to` existing) or retire self (`--retire`) | `handoff` | `handoff_agent` | ✓ | — | — | [rotation-digests](https://srjn45.github.io/warden/guides/rotation-digests/) |
| Self-rotation (retire → successor) — alias for `handoff --retire` | `rotate` | `rotate_agent` | ✓ | — | — | [rotation-digests](https://srjn45.github.io/warden/guides/rotation-digests/) |
| Fork an agent's session into a new managed agent (Codex-only; branches the conversation, source keeps running; dirty-tree carry) | `fork` | `fork_agent` | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |

## 2. Task types & worktrees

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Task `--type` (development, analysis, spike, pr-review, code, docs, website, debug-ci, tests, other) | `start --type` | `spawn_agent` (`type`) | ✓ | ✓ | — | [worktrees-task-types](https://srjn45.github.io/warden/concepts/worktrees-task-types/) |
| Managed worktree per agent | automatic | automatic | ✓ | ✓ | — | [worktrees-task-types](https://srjn45.github.io/warden/concepts/worktrees-task-types/) |
| Scratch worktree (analysis/spike) | `start --worktree` | `spawn_agent` (`worktree`) | ✓ | ✓ | — | [worktrees-task-types](https://srjn45.github.io/warden/concepts/worktrees-task-types/) |
| In-repo opt-out (write-agent) | `start --in-repo` | `spawn_agent` (`in_repo`) | ✓ | ✓ | — | [worktrees-task-types](https://srjn45.github.io/warden/concepts/worktrees-task-types/) |
| List worktrees | `worktree list` (alias `worktree ls`; bare `worktree`) | `list_worktrees` | ✓ | — | — | [worktrees-task-types](https://srjn45.github.io/warden/concepts/worktrees-task-types/) |
| Prune orphaned worktrees | `worktree prune` (alias `prune`) | `prune_worktrees` | ✓ | — | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Clean merged branches + stale worktrees | `workspace clean` (alias `clean`) | — | ✓ | — | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Remove one agent's worktree | `remove-worktree` (= `stop --keep-record`, worktree only) | `remove_worktree` | ✓ | ✓ | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |

## 3. Git & check lifecycle (with rails)

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Commit (auto-message from diff; `[paths…]`, `--amend`/`--force`) | `commit` | `commit` (`paths`, `amend`, `force`) | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Push (protected-branch rails, `--force-with-lease`) | `push` | `push` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Sync / rebase onto base (default: session/integration/default branch; `--continue` / `--abort`; non-zero on conflicts) | `sync` | `sync` (`base`, `continue`, `abort`) | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Open / return the PR for the agent branch (agent keeps running) | `git pr` (`--base`, `--title`, `--body`/`--body-file`, `--json`) | `create_pr` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Run project checks (compact failures; works from subdirectories) | `check` | `check` |
| List configured checks without running them | `check list` | `list_checks` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Agent-native diff review (backend's own reviewer) | `review` (`--base`, `--prompt`, `--backend`) | **CLI-only** (agent-native; local worktree exec, no daemon round-trip) | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |
| Machine-readable review findings (neutral JSON) | `review --json` | **CLI-only** | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |
| git-guard hook (deny raw git mutations) | `git-guard` (hook) | enforced | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| check-guard hook (redirect broad runs) | `check-guard` (hook) | enforced | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| root-guard / boundary enforcement | `guard` (hook) | enforced | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Branch + CI status (per agent) | `branches` | `get_branch_status` | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |

## 4. Pipelines (DAG of agent jobs)

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Create from YAML spec / template (`[spec.yaml]` or `-f`; `--start`, `--json`) | `pipeline create` | `create_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| List pipelines (default: current project; `--all` / `--project` / `--status` / `--json`) | `pipeline list` | `list_pipelines` | ✓ | ✓ | view | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Show one pipeline's jobs/output (`--prompts` / `--json` / `--watch` / `--all-jobs`) | `pipeline show` | `show_pipeline` | ✓ | ✓ | `i` | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Start | `pipeline start` | `start_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Cancel (confirms when jobs are live; `--yes`; cannot restart) | `pipeline cancel` | `cancel_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Pause | `pipeline pause` | `pause_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Resume | `pipeline resume` | `resume_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Show one job | `pipeline job show` | `show_pipeline` (job in payload) | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Retry a failed job | `pipeline job retry` | `retry_pipeline_job` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Edit a pending job | `pipeline job edit` | `edit_pipeline_job` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Emit a job's handoff output | `pipeline emit` | `emit_pipeline_output` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Delete a pipeline record (always confirms; `--yes`) | `pipeline delete` | `delete_pipeline` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Validate a spec (no daemon) | `pipeline validate` | `validate_pipeline` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| List built-in templates | `pipeline template list` | `list_pipeline_templates` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Plan-owned pipeline control (pipeline pause/resume/cancel/delete refuse) | `plan pause\|resume\|stop` | `control_plan` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |

## 5. Coordination (shared context, messages, conflicts)

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Shared-context set | `ctx set` | `ctx_set` | ✓ | ✓ | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Compare-and-set (claim a task) | `ctx cas` | `ctx_cas` | ✓ | — | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Append | `ctx append` | `ctx_append` | ✓ | — | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Get / list / delete | `ctx get/list/del` | `ctx_get` / `ctx_list` | ✓ | ✓ | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Directed message to an inbox | `msg send` | `send_message` | ✓ | ✓ | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Read inbox | `msg inbox` | `read_inbox` | ✓ | ✓ | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Wait for a message (park/wake) | `msg wait` | `wait_for_message` | ✓ | — | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| File-conflict detection | `collab conflicts` | `get_collaboration_status` | ✓ | ✓ | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Who is editing a file | `collab who-is-editing` | `who_is_editing_file` | ✓ | ✓ | — | [shared-context-messages](https://srjn45.github.io/warden/multi-agent/shared-context-messages/) |
| Project memory — show/edit `.warden/memory.md` | `memory` (`--raw`, `--path`, `--edit`) | **CLI-only** (local, no daemon round-trip) | ✓ | — | — | [project-memory](https://srjn45.github.io/warden/concepts/project-memory/) |
| Project memory — projected into every spawn (`memory.inject`) | config (`memory.inject`, default on) | automatic (all backends but aider) | ✓ | — | — | [project-memory](https://srjn45.github.io/warden/concepts/project-memory/) |
| Project memory — auto-curation from digests (`memory.curate`) | config (`memory.curate`, default **off**) | automatic on completion (proposes `unverified` entries to the working tree; never commits) | ✓ | — | — | [project-memory](https://srjn45.github.io/warden/concepts/project-memory/) |
| Project memory — local grounding in the REPL (`memory.ground`) | `repl` → `/memory <q>` (`/mem`, `/ask`) + `project_memory` tool | **REPL-only** (Fast-Brain, no full agent turn) | ✓ | — | — | [project-memory](https://srjn45.github.io/warden/concepts/project-memory/) |
| **Projects** — first-class daemon projects (open local/remote, close, new, list) | `projects` (`list`, `open`, `open-local`, `open-remote`, `new`, `close`) | — | — | ✓ | ✓ | [project-groups](https://srjn45.github.io/warden/guides/project-groups/) |
| **Projects — zero-touch auto-registration** — launching an agent/pipeline/terminal/plan in a directory auto-registers (or reopens) its project; `projects open*` is optional | automatic on `start` / `agent start` / `pipeline create` / `plan create` / `plan run` / terminal launch (`--project` overrides) | automatic on `spawn_agent` | — | ✓ | ✓ | [project-groups](https://srjn45.github.io/warden/guides/project-groups/) |
| **Project groups** — named collections of projects shown in the TUI tree; incremental `/members` add/remove and bulk replace | `project-groups` (`list`, `show`, `create`, `update`, `delete`, `members add\|remove`) | — | — | ✓ (read) | ✓ | [project-groups](https://srjn45.github.io/warden/guides/project-groups/) |
| **Open restores members** — opening a project restores the members that were hibernated when it was last closed; it does **not** auto-spawn an orchestrator, so a project with no restorable members opens empty | automatic on project open | — | — | — | ✓ | [project-groups](https://srjn45.github.io/warden/guides/project-groups/) |
| **Peer awareness** — grouped orchestrators learn their Project Group name and sibling orchestrator names via context injection at every (re)launch | automatic (daemon-wired via `PeerContextFn`) | — | — | — | — | [project-groups](https://srjn45.github.io/warden/guides/project-groups/) |

## 6. Approvals & permissions

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| List pending approvals | `approvals` | `list_approvals` | ✓ | ✓ | cockpit | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Answer an approval | `approve` | `approve` | ✓ | ✓ | `a` | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Auto-approve toggle | `auto-approve <id> on\|off` | `set_auto_approve` | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Auto-approve rule policy (tool/glob/regex/paths, per-agent, identical-prompt circuit breaker) | `auto-approve rules\|allow\|deny\|clear\|enable\|disable` | `set_auto_approve_policy` | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Workspace-trust prompt auto-answer (Claude/Codex/Antigravity in-pane; Cursor via `--trust`; on by default) | `config` (`trust_workspace`) | — | — | — | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Model-assisted prompt recognition (a reworded/unknown menu is read by Fast-Brain, verified against the pane, then follows the normal policy; on by default) | `config` (`recognize_prompts`) | — | — | — | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Learned prompt shapes — a model-read prompt is learned on its first verified success so the next one needs no model call; list/forget (REST `GET`/`DELETE /api/v1/known-prompts[/{id}]`; config `known_prompts_max`, `known_prompts_prune_days`) | `approval known list\|forget` | `list_known_prompts`, `forget_known_prompt` | ✓ | — | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Set permission mode (running agent) | `set-permission-mode` | `set_permission_mode` | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Supervised spawn (gate every prompt) | `start --supervised` | `spawn_agent` (`supervised`) | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |

## 7. Scheduling

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Create schedule (cron / at / now; agent or pipeline; same options as `start`) | `schedule create` | `create_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| List schedules (table with state: enabled / disabled / done / failed) | `schedule list` | `list_schedules` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Show one schedule (full fire payload + last run) | `schedule show` | `get_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Test a schedule (fire once, next run unchanged) | `schedule run` | `run_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Edit a schedule (only the flags passed) | `schedule edit` | `update_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Enable / disable schedule | `schedule enable` / `disable` | `enable_schedule` / `disable_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Delete schedule (confirms; `--yes` to skip) | `schedule delete` | `delete_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Scheduled-run session linkage (`schedule_id` on sessions; `scheduled-agents` capability) | — | — | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |

## 8. Snapshots & rollback

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Snapshot worktree + transcript | `snapshot create` | `snapshot_create` | ✓ | — | — | [snapshots](https://srjn45.github.io/warden/guides/snapshots/) |
| List snapshots | `snapshot list` | `snapshot_list` | ✓ | — | — | [snapshots](https://srjn45.github.io/warden/guides/snapshots/) |
| Restore a snapshot (rails) | `snapshot restore` | `snapshot_restore` | ✓ | — | — | [snapshots](https://srjn45.github.io/warden/guides/snapshots/) |

## 9. Observability, insights & savings

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Live resource metrics | `stats` | `get_metrics` | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Metrics history (time-series) | `stats --history` | `get_metrics` (`history`) | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Memory-pressure gate / headroom | `doctor` / spawn gate | `get_pressure` | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Fleet insights (parallelizable pairs, etc.) | `insights` | `insights` | ✓ | — | — | [insights](https://srjn45.github.io/warden/reference/insights/) |
| Cost umbrella (spend + savings in one) | `cost` (`cost spend` / `cost savings`) | `spend` + `savings` | ✓ | — | — | [savings](https://srjn45.github.io/warden/reference/savings/) |
| Token-savings ledger | `savings` (alias `cost savings`) | `savings` | ✓ | — | — | [savings](https://srjn45.github.io/warden/reference/savings/) |
| Cost governance ($ spend rollup) | `spend` (alias `cost spend`) | `spend` | ✓ | ✓ | $ in `ls` | [savings](https://srjn45.github.io/warden/reference/savings/) |
| Budget gate (soft $ cap on spawn) | config (`tokens.budget_gate`) / spawn gate | automatic | ✓ | — | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Full-text search | `search` | `search` | ✓ | ✓ | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Browse archived agents | `history` | `history` | ✓ | ✓ (archive) | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Action audit trail | `audit log` | `audit_log` | ✓ | — | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Event stream (SSE) | — | — | — | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Desktop / webhook / Slack notifications | config (`notify.enabled`, `notify.webhook_*`) | automatic | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Context/token guard (gauge, alert, auto-`/compact`) | config (`tokens.guard`) | automatic | ✓ | ✓ | gauge | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Force-compact a busy critical agent (interrupt→`/compact`→resume) | `force-compact` (config `tokens.force_compact`) | `set_force_compact` | ✓ | — | — | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Crash / anomaly detection (OOM, loop, pre-crash) | config | automatic | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |

## 10. Portability & presets

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Export session metadata | `export` | `export_sessions` | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Import session metadata | `import` | `import_sessions` | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Save a spawn preset | `preset save` / `library save-preset` | **CLI-only** (local config authoring) | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| List presets | `preset list` | **CLI-only** | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Save a prompt template | `prompt-template save` / `library save-prompt` | **CLI-only** (local config authoring) | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| List prompt templates | `prompt-template list` | **CLI-only** | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Fill a prompt template into a spawn | `start --prompt-template <name> --set VAR=value` | **CLI-only** | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |
| Browse presets + prompt templates + pipeline templates (one umbrella) | `library list` | `library_list` | ✓ | — | — | [cli](https://srjn45.github.io/warden/reference/cli/) |

## 11. Plugins (custom task types & hooks)

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| List registered plugins | `plugin list` | `list_plugins` | ✓ | — | — | [plugins](https://srjn45.github.io/warden/reference/plugins/) |
| Custom task types via plugins | config | enforced at spawn | ✓ | — | — | [plugins](https://srjn45.github.io/warden/reference/plugins/) |
| Lifecycle hook events (pre/post spawn, commit, check, teardown) | config | enforced | ✓ | — | — | [plugins](https://srjn45.github.io/warden/reference/plugins/) |

## 12. Web mission control

The browser GUI (served by the daemon) is a **URL-routed** shell (`/cockpit`
home · `/tui` · `/pipelines` · `/metrics` · `/terminals` · `/archive` · `/others`
· `/agent/<id>` — deep-linkable, back/forward, shareable). It provides: the
**Cockpit** home (Fleet header + agent grid), a full-screen **TUI** launcher
(top-bar ▢ TUI button) that streams the literal `warden tui` into the browser, a
**Pipelines** tab with a live DAG, a **Metrics**
tab (per-agent **and** fleet-total CPU/memory, per-agent context, fleet size,
tokens saved — two-column on desktop, single-column on mobile), a **Terminals**
tab (PTY viewport + New terminal; terminals are excluded from the agent grid/counts),
an **Archive** tab, the **Others** catch-all (attention queue, conflicts, activity;
sits last), in-browser **attach** terminals, a header-button **Context & Messages**
overlay, spawn modal, bulk actions, keyboard shortcuts, and theming.

| Feature | Where | Docs |
|---|---|---|
| URL routing (deep links, back/forward) | all routes | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Agent grid + Fleet header / busy-idle badges | Cockpit (`/cockpit`, home) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Attention queue / approvals | Others (`/others`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Pipelines tab + live DAG | Pipelines (`/pipelines`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Metrics: per-agent + fleet-total CPU/mem, per-agent context, fleet size, tokens saved (2-col responsive) | Metrics (`/metrics`) | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Terminals tab (PTY viewport + New terminal; excluded from the agent grid/counts) | Terminals (`/terminals`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Archive (history) | Archive (`/archive`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| In-browser attach terminal | Agent (`/agent/<id>`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Full-screen **TUI** launcher — the three-pane cockpit streamed edge-to-edge into the browser (same panes, shortcuts, real shells & live agent sessions; Ctrl+Q exits); **self-healing** (survives daemon restarts; a wedged session is auto-rebuilt, or force one with `warden tui --rebuild-web-cockpit`) | top-bar ▢ TUI button (`/tui`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Per-agent backend logo on cards + **group-by Agent** (claude/aider/…; empty ⇒ claude) | Cockpit (`/cockpit`) | [agent-backends](https://srjn45.github.io/warden/concepts/agent-backends/) |
| Context & messages | header 🗒 overlay | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Spawn modal (+ New agent) — incl. a **Role** dropdown (built-in role picker, defaults `general`) | header button | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/) |
| Bulk actions | Bulk action bar | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Conflicts panel / activity feed | Others (`/others`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Keyboard shortcuts + help | global | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| REST API + OpenAPI (`/api/docs`) | daemon | [api-openapi](https://srjn45.github.io/warden/reference/api-openapi/) |

## 13. TUI cockpit (`warden tui`)

A terminal mission-control. Keys: `n` spawn · `t` new terminal in the cursor's project · `enter` attach ·
`i` info/inspector · `a` approve · `x` terminate · `D` delete · `d` digest · `r` refresh ·
`f` filter · `g`/`G` top/bottom · `o`/`p` panes · `s` sort · `c` context ·
`M-t`/`M-a`/`M-p` rotate the terminal pane over terminals / the agent pane over agents / the agent pane over pipeline agents (add **Shift** — `M-T`/`M-A`/`M-P` — to reverse; each grabs focus on the pane it drives; or the config-free `Ctrl-b` prefix fallback — `Ctrl-b` then `t`/`a`/`p` — for terminals that don't send Alt/Option as Meta, e.g. **macOS Terminal.app / iTerm2**) ·
`?` help · `q` quit. A **◆** and a bold name badge mark the agent/terminal currently shown in a pane (tracks both `enter`-open and the `M-t`/`M-a`/`M-p` rotation).
Includes a pipeline view and per-job info.

| Feature | Where | Docs |
|---|---|---|
| Agent list + live status (incl. per-agent **backend** token; empty ⇒ claude) | control pane → Agents section | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Control-pane navigator tree — grouped by project; a project's agents, pipelines, plans and terminals nest under it | control pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Terminals nest under their project (created on demand, never auto-spawned); `t` creates a terminal in the project under the cursor | control pane → project tree / terminal pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Inspector (`i`) — agent & pipeline detail | inspector | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Approvals cockpit (`a`) | cockpit | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Digest (`d`) | inspector | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Pipeline view | pipeline pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Agent sub-trees (spawned agents nest under parent; `h`/`l` collapse; tombstone on parent delete) | main pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Spawn / attach / terminate / delete | keybindings | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Role picker in the new-agent form (`ctrl+r`; built-in catalog, defaults `general`) | new-agent form | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/) |
| Tier picker + live candidate table (`ctrl+t`; `auto` / `tier-1`/`2`/`3`; headroom bars; resolver picks backend+model) | new-agent form | [agent-roles](https://srjn45.github.io/warden/guides/agent-roles/) |
| Shift-to-select (native copy under tmux mouse mode) | help hint | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Native tmux window when launched inside tmux (no nesting; auto via `$TMUX`) | `tui --tmux-native` | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |

## 14. Orchestration surfaces

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| MCP server (stdio) | `mcp` | n/a (is the server) | ✓ | — | — | [mcp-and-skill](https://srjn45.github.io/warden/multi-agent/mcp-and-skill/) |
| `/warden` Claude skill | shipped in `skills/` | drives MCP/CLI | ✓ | — | — | [mcp-and-skill](https://srjn45.github.io/warden/multi-agent/mcp-and-skill/) |
| Interactive REPL | `repl` (aliases `interactive`, `i`) | **CLI-only** (interactive REPL) | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| ↳ deterministic `/` commands (no model) | `/agents`, `/spawn`, `/tell`, … `/help` | — | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| ↳ line editor: history, reverse-search, live `/` menu, Tab completion, colour | readline-backed | — | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| ↳ guided argument forms (pick-lists + free text, Fast-Brain pre-fill) | bare `/spawn`, `/spawn+ <prompt>` | — | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |

## 15. Backend registry (detected CLIs, tiers)

warden detects the coding-agent CLIs installed on this machine (`claude`, `codex`,
`aider`, …) and persists
each in an embedded ScrivaDB store (`~/.warden/backends`) with a billing **tier**,
an **enabled** flag, and at most one **default**. The store is warden's **single source of truth** for
which backends exist and how they're tiered: autopilot's cost-tier ladder reads from it.
warden's own internal thinking runs on **Fast-Brain** instead (thinking-mode and the reserved `local` row are retired). **Detection is a fact**
(installed / binary path / detected-at) a rescan reconciles; **tiering is a
preference** (tier / default / enabled) a rescan preserves.

Tiers: `free` · `subscription` · `pay_per_use` · `unclassified` .

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| List the registry (installed, tier, default, enabled, limited) | `backends list` (alias `ls`) | `list_backends` | ✓ | ✓ (🧩 backends panel) | `b` | [backend-registry](https://srjn45.github.io/warden/guides/backend-registry/) |
| Rescan installed CLIs (reconcile detection, preserve prefs) | `backends rescan` | `rescan_backends` | ✓ | ✓ (⟳ Rescan) | `r` | [backend-registry](https://srjn45.github.io/warden/guides/backend-registry/) |
| Set a backend's billing tier (free\|subscription\|pay_per_use\|unclassified) | `backends tier <id> <tier>` | `set_backend_tier` | ✓ | ✓ (Tier dropdown) | `t` (cycle) | [backend-registry](https://srjn45.github.io/warden/guides/backend-registry/) |
| Set the single default backend  | `backends default <id>` | `set_default_backend` | ✓ | ✓ (Default radio) | `d`/`enter` | [backend-registry](https://srjn45.github.io/warden/guides/backend-registry/) |
| Enable / disable a backend | `backends enable\|disable <id>` | — (REST `PATCH /backends/{id}`; no MCP tool) | ✓ (via CLI) | ✓ (Enabled checkbox) | `e`/space | [backend-registry](https://srjn45.github.io/warden/guides/backend-registry/) |
| Fast-Brain internal thinking (classification, naming, commit messages, digest narration, memory curation, REPL planning — latency-bounded, fail-open) | automatic | automatic | ✓ | — | — | [backend-registry](https://srjn45.github.io/warden/guides/backend-registry/) |

The registry supersedes the deprecated `autopilot.brain.backends` ladder and
`autopilot.brain.allow_pay_per_use` gate — those keys are imported **once** on the
first boot after upgrade, then ignored (edit tiers via the surfaces above).

## 16. Admin / host (CLI-only by design)

These operate on the host, the daemon process, the local shell, or the bearer
secret — they are **intentionally not exposed over MCP or web**, because doing so
would either be meaningless (process/host control) or a security smell (handing
out / rotating the very token that guards the MCP and HTTP channels).

| Feature | CLI | Why CLI-only | Docs |
|---|---|---|---|
| Run / manage the daemon | `daemon` | process control on the host | [install](https://srjn45.github.io/warden/start/install/) |
| Bearer token generate / show / rotate (+ read-only token) | `token` | the secret that protects every other surface; `show --readonly` for the view-only `WARDEN_READONLY_TOKEN` | [remote-access](https://srjn45.github.io/warden/guides/remote-access/) |
| Local-LLM model picker (memory-ranked) | `llm suggest` | reads host hardware to size the orchestrator model | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| Configuration view / init / path | `config` | local file authoring | [env-vars](https://srjn45.github.io/warden/reference/env-vars/) |
| **Data schema ledger + boot guard** (`<data>/schema.json`: integer `schema_version` separate from semver; daemon refuses a data dir it must not open — newer, needing migration, or mid-migration — before touching a store; pre-ledger installs are stamped once and boot normally; `schema_version` on `/healthz`, checked by `update`) | automatic (`daemon`; `version` prints the binary's schema) | a property of the on-disk data dir and the daemon process | [troubleshooting](https://srjn45.github.io/warden/reference/troubleshooting/) |
| **Planned upgrade & waypoint path planner** (`warden update --plan` / `wd update --plan`: evaluates release manifest, computes minimal waypoint hops, reports breaking API changes, preflights whole-data stores, and estimates downtime/disk without mutating state) | `update --plan` (alias `wd update --plan`) | host binary and data upgrade planning | [install](https://srjn45.github.io/warden/start/install/#upgrade) |
| **Migration registry runner** (`warden migrate` / `wd migrate`: `--check`, `--apply`, `--resume`, `--restore`; idempotent, journaled in schema ledger, verified before advancing ledger; daemon never runs destructive migrations at boot) | `migrate` (alias `wd migrate`) | offline data format upgrade execution | [install](https://srjn45.github.io/warden/start/install/#rollback--data-maintenance) |
| **Update transaction snapshot & rollback** (`warden rollback` / `wd rollback`: data snapshot taken in `<data>/backups/pre-<ver>-<ts>/` before update with hardlink/copy and checksums, journaled in schema ledger; rollback performs plain binary swap when no schema change, or restores snapshot with loss-of-changes confirmation; 2 updates / 14 days retention) | `rollback` (alias `wd rollback`) | transactional upgrade safety | [install](https://srjn45.github.io/warden/start/install/#rollback--data-maintenance) |
| **Whole-store repair** (`warden repair all --resolve-history=live-wins`: offline check and repair for all ScrivaDB stores, quarantine backup, resolving duplicate live keys and revision-regression conflicts) | `repair all` | offline whole-data repair; daemon must be stopped | [install](https://srjn45.github.io/warden/start/install/#rollback--data-maintenance) |
| **Data directory init & install parity** (`warden init`: stamps schema ledger, ensures all store directories, runs baseline migrations; shared code path with `scripts/install.sh`) | `init` | idempotent host data setup | [install](https://srjn45.github.io/warden/start/install/) |
| Health / environment doctor | `doctor` (`--reconcile-membership` offline membership repair) | host diagnostics | [troubleshooting](https://srjn45.github.io/warden/reference/troubleshooting/) |
| Install missing dependencies | `setup` | installs host packages (brew/apt/dnf/pacman + official installers) | [install](https://srjn45.github.io/warden/start/install/) |
| First-run tutorial | `tutorial` | interactive walkthrough | [quickstart](https://srjn45.github.io/warden/start/quickstart/) |
| Shell completion | `completion` | shell integration | [install](https://srjn45.github.io/warden/start/install/) |
| Hook entry points (guards) | `hook` / `*-guard` | invoked by Claude Code hooks | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| **Scoped factory-reset** (`--scope runtime\|data\|full`; `--backup`, `--keep-config`, `--keep-backends`, `--prune-worktrees`; requires `--yes`) | `factory-reset` | destructive host wipe; daemon must be stopped for the data phase | [cli](https://srjn45.github.io/warden/reference/cli/#warden-factory-reset) |
| Version | `version` | — | — |

---

### MCP parity summary

Every fleet/data feature is reachable over MCP (**83 tools**, including the
umbrella `stop_agent`). The only
CLI-exclusive features are the host/process/interactive/secret commands in
§16 (plus interactive `attach`/`repl`, the local-config `preset` /
`prompt-template` authoring commands, and the agent-native, worktree-local
`review` / `models` verbs — they exec in the agent's worktree with no daemon
round-trip, so they have no MCP twin by design), which are
CLI-only **by design**. New parity tools added for full coverage: `digest`,
`get_metrics`, `savings`, `spend`, `search`, `history`, `audit_log`, `list_worktrees`,
`list_plugins`, `get_pressure`, `set_auto_approve`, `set_auto_approve_policy`,
`set_permission_mode`,
`prune_worktrees`, `export_sessions`, `import_sessions`, `rotate_agent`,
`handoff_agent`, `pause_pipeline`, `resume_pipeline`, `retry_pipeline_job`,
`edit_pipeline_job`, `emit_pipeline_output`, `delete_pipeline`,
`validate_pipeline`, `list_pipeline_templates`, `library_list`,
`create_schedule`, `get_schedule`, `update_schedule`, `run_schedule`, `enable_schedule`, `disable_schedule`, `delete_schedule`, `fork_agent`, `set_role`, `list_roles`,
`list_backends`, `rescan_backends`, `set_backend_tier`, `set_default_backend`. (`set_thinking_mode` is a retired no-op.) (Enable/disable a backend is CLI/web/TUI + REST `PATCH
/backends/{id}` only — intentionally not an MCP tool.)
