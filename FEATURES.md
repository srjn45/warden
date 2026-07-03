# warden — Feature Catalog

The authoritative inventory of **every** warden capability and where you can drive
it. warden exposes its features across five surfaces:

- **CLI** — the `warden` binary (aliased `wd`); always available.
- **MCP** — structured tools for an orchestrating agent (`warden mcp`); **69 tools**.
- **Skill** — the `/warden` Claude Code skill that prefers MCP, falls back to CLI.
- **Web** — the browser mission-control GUI (`warden daemon` + the web app).
- **TUI** — the terminal cockpit (`warden tui`).

**Coverage legend:** ✓ supported · — not applicable / not present on that surface ·
**CLI-only** features are marked and explained (host/process/interactive/secret
operations that are meaningless or unsafe over MCP/web).

> This file is the quick **coverage matrix** (what each surface can drive). For the
> in-depth prose description of how each subsystem works, see
> [`docs/FEATURES.md`](docs/FEATURES.md); for day-to-day usage,
> [`docs/USAGE.md`](docs/USAGE.md).

> Maintenance: keep the matrices truthful by cross-checking against
> `internal/cli/root.go` (CLI commands), `internal/mcp/server.go` +
> `internal/mcp/tools_extra.go` (MCP tools), and the website nav in
> `site/astro.config.mjs`. This file is mirrored on the website at
> **Reference → Feature catalog**.

---

## 1. Agent lifecycle

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
| Restore an orphaned agent | `restore` | `restore_agent` | ✓ | ✓ | — | [agents-lifecycle](https://srjn45.github.io/warden/concepts/agents-lifecycle/) |
| Delete / hard-purge | `delete` (= `stop --keep-worktree`, record only) | `delete_agent` | ✓ | ✓ | `D` | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Rename an agent | `adopt --name` / spawn `name` | `spawn_agent` (`name`) | ✓ | ✓ | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Tags (group / filter) | `start --tag`, `ls --tag` | `spawn_agent` (`tags`) | ✓ | ✓ | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |
| Model selection | `start --model` / config | `spawn_agent` (`model`) | ✓ | ✓ | — | [env-vars](https://srjn45.github.io/warden/reference/env-vars/) |
| List the backend's live model menu | `models` (`--backend`, `--json`) | **CLI-only** (agent-native; local worktree exec, no daemon round-trip) | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |
| Backend selection (Claude / Aider / OpenCode / Codex / Crush / Goose / Cursor / Antigravity) | `start --backend` | `spawn_agent` (`backend`) | ✓ | — | — | [agent-backends](https://srjn45.github.io/warden/concepts/agent-backends/) — only `claude` is stable; `aider`, `opencode`, `codex`, `crush`, `goose`, `cursor`, and `antigravity` are 🧪 experimental |
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
| Remove one agent's worktree | `remove-worktree` (= `stop --keep-record`, worktree only) | `remove_worktree` | ✓ | ✓ | — | [fleet-operations](https://srjn45.github.io/warden/guides/fleet-operations/) |

## 3. Git & check lifecycle (with rails)

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Commit (auto-message from diff) | `commit` | `commit` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Push (protected-branch rails) | `push` | `push` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Sync / rebase onto base | `sync` | `sync` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Run project checks (compact failures) | `check` | `check` | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Agent-native diff review (backend's own reviewer) | `review` (`--base`, `--prompt`, `--backend`) | **CLI-only** (agent-native; local worktree exec, no daemon round-trip) | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |
| Machine-readable review findings (neutral JSON) | `review --json` | **CLI-only** | ✓ | — | — | [backend-superpowers](https://srjn45.github.io/warden/guides/backend-superpowers/) |
| git-guard hook (deny raw git mutations) | `git-guard` (hook) | enforced | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| check-guard hook (redirect broad runs) | `check-guard` (hook) | enforced | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| root-guard / boundary enforcement | `guard` (hook) | enforced | ✓ | — | — | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Branch + CI status (per agent) | `branches` | `get_branch_status` | ✓ | ✓ | — | [observability](https://srjn45.github.io/warden/reference/observability/) |

## 4. Pipelines (DAG of agent jobs)

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Create from YAML spec / template | `pipeline create` | `create_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| List pipelines | `pipeline list` | `list_pipelines` | ✓ | ✓ | view | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Show one pipeline's jobs/output | `pipeline show` | `show_pipeline` | ✓ | ✓ | `i` | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Start | `pipeline start` | `start_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Cancel | `pipeline cancel` | `cancel_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Pause | `pipeline pause` | `pause_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Resume | `pipeline resume` | `resume_pipeline` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Retry a failed job | `pipeline retry` | `retry_pipeline_job` | ✓ | ✓ | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Edit a pending job | `pipeline edit-job` | `edit_pipeline_job` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Emit a job's handoff output | `pipeline emit` | `emit_pipeline_output` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Delete a pipeline record | `pipeline delete` | `delete_pipeline` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| Validate a spec (no daemon) | `pipeline validate` | `validate_pipeline` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |
| List built-in templates | `pipeline list-templates` | `list_pipeline_templates` | ✓ | — | — | [pipelines](https://srjn45.github.io/warden/multi-agent/pipelines/) |

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
| Project memory — projected into every spawn (`memory_inject`) | config (`memory_inject`, default on) | automatic (all backends but aider) | ✓ | — | — | [project-memory](https://srjn45.github.io/warden/concepts/project-memory/) |
| Project memory — auto-curation from digests (`memory_curate`) | config (`memory_curate`, default **off**) | automatic on completion (proposes `unverified` entries to the working tree; never commits) | ✓ | — | — | [project-memory](https://srjn45.github.io/warden/concepts/project-memory/) |

## 6. Approvals & permissions

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| List pending approvals | `approvals` | `list_approvals` | ✓ | ✓ | cockpit | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Answer an approval | `approve` | `approve` | ✓ | ✓ | `a` | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Auto-approve toggle | `auto-approve <id> on\|off` | `set_auto_approve` | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Auto-approve rule policy (tool/glob/regex/paths, per-agent, identical-prompt circuit breaker) | `auto-approve rules\|allow\|deny\|clear\|enable\|disable` | `set_auto_approve_policy` | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Set permission mode (running agent) | `set-permission-mode` | `set_permission_mode` | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |
| Supervised spawn (gate every prompt) | `start --supervised` | `spawn_agent` (`supervised`) | ✓ | ✓ | — | [approvals-supervised](https://srjn45.github.io/warden/guides/approvals-supervised/) |

## 7. Scheduling

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| Create schedule (cron / at; agent or pipeline) | `schedule create` | `create_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| List schedules | `schedule list` | `list_schedules` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |
| Delete schedule | `schedule delete` | `delete_schedule` | ✓ | — | — | [scheduling](https://srjn45.github.io/warden/guides/scheduling/) |

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
home · `/tui` · `/pipelines` · `/metrics` · `/archive` · `/others` · `/agent/<id>`
— deep-linkable, back/forward, shareable). It provides: the **Cockpit** home
(Fleet header + agent grid), a full-screen **TUI** launcher (top-bar ▢ TUI
button) that streams the literal `warden tui` into the browser, a **Pipelines**
tab with a live DAG, a **Metrics**
tab (per-agent **and** fleet-total CPU/memory, per-agent context, fleet size,
tokens saved — two-column on desktop, single-column on mobile), an **Archive**
tab, the **Others** catch-all (attention queue, conflicts, activity; sits last),
in-browser **attach** terminals, a header-button **Context & Messages** overlay,
spawn modal, bulk actions, keyboard shortcuts, and theming.

| Feature | Where | Docs |
|---|---|---|
| URL routing (deep links, back/forward) | all routes | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Agent grid + Fleet header / busy-idle badges | Cockpit (`/cockpit`, home) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Per-agent backend logo on cards + **group-by Agent** (claude/aider/…; empty ⇒ claude) | Cockpit (`/cockpit`) | [agent-backends](https://srjn45.github.io/warden/concepts/agent-backends/) |
| Attention queue / approvals | Others (`/others`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Pipelines tab + live DAG | Pipelines (`/pipelines`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Metrics: per-agent + fleet-total CPU/mem, per-agent context, fleet size, tokens saved (2-col responsive) | Metrics (`/metrics`) | [observability](https://srjn45.github.io/warden/reference/observability/) |
| Archive (history) | Archive (`/archive`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| In-browser attach terminal | Agent (`/agent/<id>`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Full-screen **TUI** launcher — the three-pane cockpit streamed edge-to-edge into the browser (same panes, shortcuts, real shells & live agent sessions; Ctrl+Q exits) | top-bar ▢ TUI button (`/tui`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Context & messages | header 🗒 overlay | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Spawn modal (+ New agent) | header button | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Bulk actions | Bulk action bar | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Conflicts panel / activity feed | Others (`/others`) | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| Keyboard shortcuts + help | global | [web-mission-control](https://srjn45.github.io/warden/guides/web-mission-control/) |
| REST API + OpenAPI (`/api/docs`) | daemon | [api-openapi](https://srjn45.github.io/warden/reference/api-openapi/) |

## 13. TUI cockpit (`warden tui`)

A terminal mission-control. Keys: `n` spawn · `enter` attach · `i` info/inspector ·
`a` approve · `x` terminate · `D` delete · `d` digest · `r` refresh · `f` filter ·
`g`/`G` top/bottom · `o`/`p` panes · `s` sort · `c` context · `tab` switch view ·
`?` help · `q` quit. Includes a pipeline view and per-job info.

| Feature | Where | Docs |
|---|---|---|
| Agent list + live status (incl. per-agent **backend** token; empty ⇒ claude) | main pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Inspector (`i`) — agent & pipeline detail | inspector | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Approvals cockpit (`a`) | cockpit | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Digest (`d`) | inspector | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Pipeline view | pipeline pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Agent sub-trees (spawned agents nest under parent; `h`/`l` collapse; tombstone on parent delete) | main pane | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Spawn / attach / terminate / delete | keybindings | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |
| Shift-to-select (native copy under tmux mouse mode) | help hint | [tui-cockpit](https://srjn45.github.io/warden/guides/tui-cockpit/) |

## 14. Orchestration surfaces

| Feature | CLI | MCP | Skill | Web | TUI | Docs |
|---|---|---|---|---|---|---|
| MCP server (stdio) | `mcp` | n/a (is the server) | ✓ | — | — | [mcp-and-skill](https://srjn45.github.io/warden/multi-agent/mcp-and-skill/) |
| `/warden` Claude skill | shipped in `skills/` | drives MCP/CLI | ✓ | — | — | [mcp-and-skill](https://srjn45.github.io/warden/multi-agent/mcp-and-skill/) |
| Interactive mode / REPL | `repl` (aliases `interactive`, `i`) | **CLI-only** (interactive REPL) | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| ↳ deterministic `/` commands (no model) | `/agents`, `/spawn`, `/tell`, … `/help` | — | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| ↳ line editor: history, reverse-search, Tab completion, colour | readline-backed | — | — | — | — | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |

## 15. Admin / host (CLI-only by design)

These operate on the host, the daemon process, the local shell, or the bearer
secret — they are **intentionally not exposed over MCP or web**, because doing so
would either be meaningless (process/host control) or a security smell (handing
out / rotating the very token that guards the MCP and HTTP channels).

| Feature | CLI | Why CLI-only | Docs |
|---|---|---|---|
| Run / manage the daemon | `daemon` | process control on the host | [install](https://srjn45.github.io/warden/start/install/) |
| Bearer token generate / show / rotate (+ read-only token) | `token` | the secret that protects every other surface; `show --readonly` for the view-only `WARDEN_READONLY_TOKEN` | [remote-access](https://srjn45.github.io/warden/guides/remote-access/) |
| Configuration view / init / path | `config` | local file authoring | [env-vars](https://srjn45.github.io/warden/reference/env-vars/) |
| Health / environment doctor | `doctor` | host diagnostics | [troubleshooting](https://srjn45.github.io/warden/reference/troubleshooting/) |
| Install missing dependencies | `setup` | installs host packages (brew/apt/dnf/pacman + official installers) | [install](https://srjn45.github.io/warden/start/install/) |
| Local-LLM model picker (memory-ranked) | `llm suggest` | reads host hardware to size the orchestrator model | [repl](https://srjn45.github.io/warden/multi-agent/repl/) |
| First-run tutorial | `tutorial` | interactive walkthrough | [quickstart](https://srjn45.github.io/warden/start/quickstart/) |
| Shell completion | `completion` | shell integration | [install](https://srjn45.github.io/warden/start/install/) |
| Hook entry points (guards) | `hook` / `*-guard` | invoked by Claude Code hooks | [lifecycle-and-rails](https://srjn45.github.io/warden/guides/lifecycle-and-rails/) |
| Version | `version` | — | — |

---

### MCP parity summary

Every fleet/data feature is reachable over MCP (**69 tools**, including the
umbrella `stop_agent`). The only
CLI-exclusive features are the host/process/interactive/secret commands in
§15 (plus interactive `attach`/`repl`, the local-config `preset` /
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
`create_schedule`, `delete_schedule`, `fork_agent`.
