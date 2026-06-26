# warden Features

A consolidated catalog of everything `warden` can do, grouped by area. This is
the "what exists" reference — for *how to use it* day to day see
[USAGE.md](USAGE.md), and for build/install details see the
[README](../README.md).

> `warden` is aliased as `wd`; every CLI feature below works under either name.

---

## 1. Core architecture

One self-contained Go binary that wears several faces, all sharing the same
on-disk state:

| Capability | Description |
|---|---|
| **Single-binary distribution** | `warden` bundles the daemon, CLI clients, MCP server, TUI, and (in release builds) the embedded web GUI. `wd` is an installed symlink. |
| **Local daemon** | The single writer to the session store. Serves a loopback REST API (`127.0.0.1:8765`) and runs a background poller that keeps each agent's status and subject fresh. |
| **File-based JSON store** | Sessions persisted as JSON files under `~/.warden` (`sessions/`, `closed/`) — no database to run. |
| **Claude Code lifecycle hooks** | A hook script posts `SessionStart`/`Notification`/`Stop`/`SubagentStop`/`SessionEnd` to the daemon so status updates in real time without polling. Fails soft (never blocks the agent). |
| **launchd auto-start (macOS)** | Installs as an auto-starting, crash-restarting background service. |
| **Stable code identity** | One-time self-signed code-signing cert keeps the macOS TCC (Full Disk Access) grant stable across rebuilds. |
| **Security hardening** | `0700` data dir, slowloris/body/write timeouts (bypassed for SSE/WS/long-poll), refuses non-loopback bind unless the `allow_nonloopback` config setting is true. |
| **`warden doctor`** | Preflight checks: required binaries (`tmux`, `git`, `claude`, `gh`), daemon reachability, data directory. |
| **`warden version`** | Prints version + build metadata (commit, build date, Go version, platform); `--version` shows the same, `version --json` for scripting. Stamped via ldflags (goreleaser + `make build`) with a VCS-stamp fallback. |

---

## 2. Spawning agents

| Feature | Description |
|---|---|
| **Prompt-spawn** | `warden start "<prompt>"` — no repo or type needed. Runs `claude` in the caller's directory (or `--dir`). |
| **Auto-classification** | The daemon classifies a prompt-spawned agent's type with `claude -p` shortly after creation (falls back to `other`). |
| **Auto-generated subject** | Each agent gets a one-line ≤8-word summary of what it's doing, seeded from the prompt and refreshed by the poller from the transcript or tmux pane (throttled, change-gated). |
| **Managed worktree spawn** | `--type` creates/adopts a git worktree where the type needs one. |
| **Worktree adoption** | If a worktree for the ticket already exists, the spawn reattaches to it instead of erroring. |
| **Configurable permission mode** | Per-agent and global control over Claude permission level. CLI flag: `--permission-mode <mode>` (values: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`). Legacy alias: `--supervised` (equivalent to `--permission-mode acceptEdits`). Global default: the `default_permission_mode` config setting (defaults to `auto`). Runtime change: `warden set-permission-mode <id> <mode>`. Display: PERMISSION_MODE column in `warden ls`, permission_mode field in `warden status`. Stored in session: mode preserved on restore/resume. Empty mode means "use global default" and displays as `default`. |
| **Model selection** | Per-agent model selection via `--model` flag (CLI and MCP). Short aliases for common models: `opus`, `sonnet`, `haiku`, `fable`. Config default: the `model_default` setting. Fallback: `claude-sonnet-4-6` if not specified. Display: MODEL column in `warden ls`, model field in `warden status`. Stored in session: model preserved on restore/resume. |
| **Spawn presets** | Save reusable spawn defaults under a name and replay them. `warden preset save <name> [spawn flags]` persists `--type`/`--model`/`--permission-mode` (`--supervised`)/`--auto-restart`/`--worktree`/`--in-repo` to `~/.warden/presets.yaml`; `warden preset list` shows them. `warden start --preset <name>` seeds those defaults, and any explicit CLI flag still overrides the preset. Per-invocation inputs (ticket, branch, PR, dir) are not stored. |
| **Prompt templates** | Save reusable, variabled prompt *bodies* (where presets store flags). `warden prompt-template save <name> --prompt "…{{VAR}}…"` (alias `pt`) persists a prompt body with `{{VAR}}` placeholders to `~/.warden/prompt-templates.yaml`; the declared variables are auto-derived from the body. `warden prompt-template list` shows each template and its variables. `warden start --prompt-template <name> --set FILE=foo.go --set X=y` resolves the template into the spawn prompt (every declared variable must be supplied; an unknown `--set` is rejected). An explicit positional prompt still wins, and `--prompt-template` is free-form only (no `--type`). |
| **Library umbrella** (`warden library`, alias `lib`) | One discoverable entry point over all three kinds of reusable launch config: spawn **presets**, **prompt templates**, and the built-in pipeline **templates**. `warden library list` shows all three in labeled sections (presets + their stored defaults, prompt templates + their variables, and pipeline templates + a short description); `warden library save-preset <name> [spawn flags]` delegates to `warden preset save` and `warden library save-prompt <name> --prompt "…"` delegates to `warden prompt-template save`. Purely additive — no new storage or format: it reuses the existing preset store, the prompt-template store, and the embedded template catalog, and the standalone `preset`, `prompt-template`, and `pipeline list-templates` commands keep working unchanged. Pipeline templates are embedded/read-only, so there is no `save-template` (author a pipeline from a YAML spec with `pipeline create -f`). Also exposed over MCP as `library_list` (returns `{presets, prompt_templates, templates}`). |

### Task types (`--type`)

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | `.worktrees/<ticket>` on a branch named after the ticket |
| `pr-review` | yes (PR branch) | Detached worktree; runs `gh pr checkout <PR>`. Needs `--pr`/`--branch` |
| `analysis` | opt-in (`--worktree`) | Runs in the repo by default |
| `spike` | opt-in (`--worktree`) | Same as analysis |
| `code` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `docs` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `website` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `debug-ci` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `tests` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `other` | no | Catch-all / unrecognized type strings |

---

## 3. Lifecycle management

| Command / feature | Description |
|---|---|
| `stop` | **The single umbrella teardown verb.** Default `wd stop <TICKET>` = full teardown: terminate the session, clear (archive) the record, **and** remove the git worktree + branch (asks for confirmation first unless `--yes`). Subtractive flags: `--keep-record`, `--keep-worktree` (`--keep-worktree` alone == the old `done`), `--hard` (purge record), `--pr`/`--base` (open a GitHub PR first while the agent is intact), `--force`/`--delete-adopted-branch` (worktree guards). Safe order: PR → terminate → clear record → remove worktree, so a failed push leaves the agent running. |
| `terminate` | Stop an agent (kill tmux + claude); **keeps** the record and worktree. The safe, reversible "stop" default. Alias for `stop --keep-record --keep-worktree`. |
| `restore` | Recreate and resume a lost/orphaned agent's session (`claude --resume`). |
| `done` | Terminate **and** clear the record in one step (worktree kept). Alias for `stop --keep-worktree`. `--hard` purges instead of archiving. `--create-pr` first pushes the agent's branch and opens a GitHub PR (`gh`) titled from the agent and bodied from its digest (`--base` sets the target, default main) — the PR is opened *before* termination, so a failure leaves the agent running to retry; an existing PR for the branch is reported, not re-created. |
| `delete` | Clear the stored record (archive by default, `--hard` purge). Leaves tmux + worktree alone. Alias for `stop --keep-worktree` (record only). |
| `remove-worktree` | Remove the git worktree + branch. **Destructive** — refuses while the agent runs or has uncommitted/unpushed work unless `--force`. Alias for `stop --keep-record` (worktree only); always asks unless `--yes`. |
| `worktree ls` | List warden-owned worktrees under `.worktrees`, joined to active/archived records (provenance-tracked). |
| `prune` | Reclaim orphaned warden worktrees (always prompts; `--force` overrides guards, `--include-archived` widens scope). Retention is policy-driven via the `worktree_keep_done` / `worktree_auto_prune` config settings. |
| `adopt` | Register an existing Claude session — resume newest-for-dir under tmux, or live-register a running tmux session. |
| **Cascade cleanup** | Deleting a pipeline/agent cascades cleanup of its shared-context keys and (on hard-delete) its mailbox inbox. |

---

## 4. Observation & interaction

| Command / feature | Description |
|---|---|
| `ls` | List active agents (type, status, age, dir, subject). `--json` for machine output. `--watch`/`-w` live-updates the table on every agent state change over the daemon's SSE stream (Ctrl+C to exit); combine with `--json` to stream one JSON snapshot per change. |
| `status <id>` | Full detail for one agent — workdir, subject, worktree, branch, PR, events. `--json` available. |
| `attach <id>` | Attach your terminal to the agent's tmux session interactively. |
| `send <id> <msg>` | Type a message into the agent's claude session and press Enter. |
| `tail <id>` | Print recent terminal output (`--lines N`). |
| `digest <id>` | Completion digest — files touched, branch, turn count, and a best-effort `claude -p` narrative. `--json` available. |
| **Stuck / attention detection** | Agents flagged `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`, surfaced across all interfaces. |

---

## 5. Approvals inbox

Answer routine Claude tool-permission prompts (from supervised agents) without
attaching. Controlled by the `approvals` config setting (on by default).

| Surface | How |
|---|---|
| **CLI** | `warden approvals` lists recognized pending prompts with their numbered options; `warden approve <id> <n>` answers one. |
| **Web** | One-click option buttons in the AttentionQueue. |
| **TUI** | A pinned **⏳ Approvals** row (`i` / `enter`, then `1`-`9`; `tab` cycles agents). |
| **Safety** | A TOCTOU re-capture + fingerprint re-verify guards answers; unrecognized prompts always fall back to attach. |

### Auto-Approve

Automatically approve yes/no tool-permission prompts by always selecting option 1.
Off by default (opt-in safety), enabled globally via the `auto_approve` config
setting or per-agent via `warden auto-approve <id> on|off`.

**Behavior:**
- Only triggers for recognized yes/no prompts (parsed via `approval.Parse`)
- Always selects option 1 (predictable, auditable behavior)
- Skips multi-select, text-entry, and unrecognized prompts (falls back to manual approval)
- Logs all auto-approval attempts (success/skip/failure) for auditing
- Per-agent setting overrides global default

**Configuration:**
```yaml
# Enable globally (all supervised agents): in ~/.warden/config.yaml, then restart the daemon
auto_approve: true
```
```bash
# Toggle for a specific agent (overrides the global default)
warden auto-approve abc123 on   # enable for agent abc123
warden auto-approve abc123 off  # disable for agent abc123
```

**Safety:**
- Off by default (must explicitly enable)
- Only works with recognized prompt grammar (strict parser)
- Never retries on failure (fail-safe to manual approval)
- Does not bypass approvals inbox (works alongside it)

---

## 6. Multi-agent collaboration

| Feature | Description |
|---|---|
| **Pipelines** (`warden pipeline`) | YAML-defined **DAG of dependent agent jobs**. The daemon runs them: dependency-free jobs start first, each job's `emit` publishes output and unblocks dependents — keeping the lead Claude off the critical path. Sub-commands: `validate` (client-side spec check, CI-friendly exit codes), `create` (from a spec file **or** a built-in `--template`), `list-templates`, `start`, `pause`/`resume` (halt new spawns while in-flight jobs finish), `show`, `list`, `edit-job`, `retry`, `cancel`, `delete`. Drivable from the CLI, the MCP tools (create/start/show/cancel/list), and with full TUI + web visibility (DAG view). |
| **Pipeline templates** | Four `go:embed`-bundled starters — `analyze-implement-review`, `parallel-tasks`, `test-fix-verify`, `research-synthesis` — render via `warden pipeline create --template <name>` with `{{NAME}}`/`{{REPO}}` (auto-filled) and `--set KEY=VALUE` placeholder substitution. `warden pipeline list-templates` lists them and their placeholders. |
| **Conditional steps** (`run_if`) | Per-job `run_if: success\|failure\|always` (default `success`). A job runs only when its dependencies settled the right way — `failure`/`always` handlers let a pipeline route around a failed upstream and still complete, and the handler's prompt is told which upstream failed. |
| **Shared context** (`warden ctx`) | A namespaced key/value blackboard all agents can read/write: `ctx set`/`get`/`list`. |
| **Directed messages** (`warden msg`) | Per-agent inbox: `msg send` (wakes a parked idle/waiting agent), `msg inbox`, `msg wait` (blocks in the daemon until a message arrives). |
| **File-conflict detection** (`warden collab`) | The daemon watches each active agent's worktree and warns (via the inbox, deduplicated) when two agents are editing the same file. Detection is **real-time**: an fsnotify watcher reacts to edits in subseconds (a burst of saves is debounced into one scan), while a slower `git diff` poll reconciles the watch set against the active-session view and acts as a safety net — degrading cleanly to pure polling if fsnotify is unavailable or the inotify watch budget (80% of the per-user limit) is exhausted. Inspect with `collab conflicts` / `collab who-is-editing <file>`, `GET /api/v1/collab/conflicts`, the `get_collaboration_status` / `who_is_editing_file` MCP tools, or the **File conflicts** card on the dashboard's Others tab. Spawned agents also get a system-prompt hint to check `who_is_editing_file` and their inbox before editing shared files, so they coordinate rather than overwrite. Tunable via `collab_enabled` / `collab_interval` / `collab_hint`. |
| **Branch tracking** (`warden branches`) | Opt-in daemon monitor that reports, per active agent with a branch, its **GitHub CI status** (latest `gh run list` inside the worktree → success/failure/pending/none) and its **standing vs `origin/main`** (commits behind/ahead, and whether already merged). Alerts are **informational, never blocking**: a newly-observed CI failure delivers an inbox note to the agent **and** a desktop notification to the operator (desktop is reserved for CI failures); a merged branch or one fallen >10 commits behind delivers an inbox nudge. A 5-minute dedup window keyed on `(branch, signal-state)` suppresses repeats but re-alerts on a state change (pending→failure). Every subprocess **fails open** — a missing/unauthenticated `gh`, a timeout, or a non-repo worktree simply skips that branch for the tick. Inspect read-only via `warden branches` (`--json`), `GET /api/v1/collab/branches`, or the `get_branch_status` MCP tool. Off by default; enable with `branch_track_enabled` / tune `branch_track_interval` (default `2m`). |

---

## 7. Handoff — one verb (`warden handoff`)

`warden handoff` is the single concept for passing work to another agent. It has
three modes, distinguished by who runs the work next and whether the caller
survives. Phase 1 (writing the handoff file + resume prompt) is driven by the
`/warden` skill; this verb performs the delivery.

- **New delegate (default)** — spawns a fresh delegate in its **own isolated
  worktree** for a sub-task; the source agent **keeps running**.
- **Existing agent (`--to <id>`)** — delivers the handoff into an already-running
  agent's inbox (waking it); the source agent **keeps running**.
- **Retire self (`--retire`, requires `--confirm`)** — spawns a successor in the
  calling agent's **same workdir/worktree**, then reaps the caller
  (self-succession). This is what the `warden rotate` **alias** runs.

`--retire` and `--to` are **mutually exclusive** (one reaps the caller, the other
never does). Invariants enforced at compile time via the minimal client
interfaces each mode uses:

- The **retire** path **reuses the worktree by cwd and never removes it** (the
  rotator interface omits worktree removal). **Spawn-before-reap** is fail-safe —
  if the successor fails to spawn, the caller keeps running.
- The **delegate / `--to`** paths **never terminate the source** (the handoff
  interface omits termination). For `--to`, the handoff file's **content is
  inlined** into the message — the recipient runs in a different worktree and
  can't read the source's file by path.

`warden rotate` and the `rotate_agent` MCP tool remain as **thin aliases** for the
retire mode (`warden handoff --retire` / `handoff_agent {retire:true}`) — same
flags, same behavior.

---

## 8. Terminal UI (cockpit)

`warden tui` (or bare `warden`) opens a **tmux-composited cockpit** with three
panes: an agents list, a terminal shell for CLI access, and a full-height live
detail pane for the selected agent.

| Feature | Description |
|---|---|
| **Live list** | Polls the daemon ~1×/sec; browse with `↑`/`↓` without disturbing the detail pane. |
| **Pipeline tree** | Pipelines shown as a collapsible `▸ Pipelines` section; expand/collapse, open running jobs, retry failed jobs. |
| **Directory groups** | `o` opens a directory as a group (becomes the spawn target for `n`), with `/fs/dirs` tab-completion. |
| **In-cockpit actions** | `n` new agent, `s` send, `a` attach (full-screen), `d` digest overlay, `i` approvals, `c` context/message inspector, `x` terminate/cancel, `D` delete pipeline record, `?` help. |
| **Terminal shell pane** | Bottom-left pane runs `$SHELL` for direct CLI access to `warden` commands and other terminal work. |
| **Pane focus** | Move focus with `Alt+←/→/↑/↓` (no tmux prefix). |
| **Native scrolling** | Per-agent tmux sessions enable `mouse on` + raised `history-limit` for wheel/copy-mode scrolling of long output. |

> Requires tmux ≥ 3.1; must run from a plain terminal (not nested inside tmux).

---

## 9. Web GUI

The daemon embeds a React (Astro) dashboard at `http://localhost:8765` — no
separate server.

| Feature | Description |
|---|---|
| **URL-routed shell** | Tabs are real URLs via the History API — `/cockpit` (home), `/pipelines`, `/metrics`, `/archive`, `/others` (catch-all, last), and `/agent/<id>` per pinned agent. Back/forward, refresh, and shareable deep links all work; `/` redirects to `/cockpit`. |
| **Cockpit (home)** | Default view: a slim **Fleet** header (totals · busy · waiting · errored, pressure, per-dir counts) above the canonical agent grid. The old *Quick spawn* widget and duplicate *All agents* mini-grid were removed. |
| **Others (catch-all)** | The former *Overview*, renamed: holds *Needs you* (attention queue with one-click approvals), *File conflicts*, and *Recent activity*. The landing spot for any not-yet-homed widget. |
| **Live fleet over SSE** | No manual refresh; coloured busy/idle badges (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) + each agent's subject. |
| **Metrics tab** | A responsive grid of uPlot chart cards — **two columns** on wide screens (each per-agent chart sits beside its fleet-wide total), a **single column** on phones: **CPU per agent** + **Total CPU**, **Memory per agent** (GiB) + **Total memory**, **Context per agent** (client-accumulated time series of live context fill, legend dot colored `ok`/`warning`/`critical`; in-session only — resets on full reload), **Number of agents** (fleet size over time), **Tokens saved** (the saved-tokens trend from the savings ledger with a **window picker** — `24h`/`48h` bucket by hour, `7d`/`30d`/`All` by day — so a fresh ledger plots a real curve not a single point; a per-bucket saved area + a running-cumulative line + a headline saved-tokens/$ figure; a "set `savings: true`" hint when the ledger is disabled), and **Savings by feature** (a per-feature stacked-area breakdown of that trend). A full-width **Live footprint** card carries the former Resources panel. |
| **Context & Messages overlay** | No longer a tab — opened from a small **🗒 header button** as a dismissible overlay (Esc closes). |
| **Agent grouping** | The Cockpit grid buckets agents into collapsible panes by **Directory / Type / Status / Tag** (multi-tagged agents appear under each tag; untagged agents bucket together). The choice is saved to LocalStorage. |
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. |
| **Create agent** | **+ New agent** prompt box with a directory picker (live prefix autocomplete) and a **Supervised** checkbox. |
| **Terminate with git guard** | Surfaces a 409 → **Force** + optional hard-delete when there's uncommitted/unpushed work. |
| **Digest panel** | View an agent's completion digest in the browser. |
| **Pipeline DAG view** | Pipelines rendered as their dependency graph (`PipelineDag`). |
| **Browser notifications** | Opt-in desktop notification when an agent enters `waiting_for_input` (gated to hidden tabs). |
| **Theme toggle (light/dark/system)** | Header toggle cycles **System → Light → Dark**, defaulting to System (follows the OS via `prefers-color-scheme`). The choice persists in `localStorage` (`warden.theme`) and is applied before first paint to avoid a flash. The palette is themed through CSS custom properties keyed on `<html data-theme>`, which also pins `color-scheme` so system colors flip on an explicit override; the wordmark swaps to match the resolved theme. |
| **Keyboard shortcuts** | Global shortcut layer: `?` toggles a help overlay (also reachable from the header `?` button), `n` new agent, `/` focus the agent filter, `r` refresh the fleet, `1`–`9` jump to a tab, `j`/`k` next/previous tab, `Esc` close a modal/overlay or blur a field. Single-key bindings stay dormant while typing in a field (Esc still fires). |

---

## 10. Orchestration (MCP)

`warden mcp` is a stdio MCP server so an orchestrator Claude session can manage
the fleet through tool calls. **66 tools** are exposed — every fleet/data feature
the CLI has, so the skill/MCP can drive warden at full parity (only the
host/process/interactive/secret commands in the [feature catalog](../FEATURES.md)
stay CLI-only). Tools exposed:

| Tool(s) | Purpose |
|---|---|
| `list_agents` / `get_agent` | List agents / full detail for one |
| `spawn_agent` | Spawn (prompt mode or `type`+`repo`; `supervised` opt-in) |
| `adopt_agent` | Register an existing Claude session |
| `send_to_agent` / `get_agent_output` | Type into / read recent output of an agent |
| `digest` | Compact catch-up summary of one agent |
| `stop_agent` | **Umbrella teardown.** Default = full teardown (terminate + clear record + remove worktree); `keep_record` / `keep_worktree` subtract steps, `hard` purges, `pr`/`base` open a PR first, `force`/`delete_adopted_branch` for the worktree guards |
| `terminate_agent` / `restore_agent` | Stop (reversible) / resume an agent |
| `delete_agent` / `remove_worktree` | Clear record / remove worktree (guarded) |
| `list_worktrees` / `prune_worktrees` | List / reconcile a repo's worktrees |
| `handoff_agent` / `rotate_agent` | Hand off work — delegate to new/`to` existing agent, or `retire`→successor in place; `rotate_agent` is an alias for `handoff_agent {retire:true}` |
| `ctx_set` / `ctx_cas` / `ctx_append` / `ctx_get` / `ctx_list` | Shared-context blackboard |
| `send_message` / `read_inbox` / `wait_for_message` | Directed messaging (park/wake) |
| `get_collaboration_status` / `who_is_editing_file` | File-conflict detection |
| `get_branch_status` | Per-agent CI + branch-vs-main status |
| `list_approvals` / `approve` | List / answer pending tool-permission prompts |
| `set_auto_approve` / `set_permission_mode` | Auto-approve toggle / permission-mode change |
| `commit` / `push` / `sync` | Git lifecycle on the agent's pinned worktree — staged commit (auto-message when omitted), push, rebase-sync — returning compact structs instead of raw git output (see §22) |
| `check` | Run the project's `.warden/check.yml` checks, returning pass/fail with output for only the failing ones (see §22) |
| `create_pipeline` / `list_pipelines` / `show_pipeline` | Create a DAG pipeline from a YAML spec / list / inspect (jobs, branches, handoffs) |
| `start_pipeline` / `cancel_pipeline` / `pause_pipeline` / `resume_pipeline` | Run / cancel / pause / resume a pipeline |
| `retry_pipeline_job` / `edit_pipeline_job` / `emit_pipeline_output` / `delete_pipeline` | Per-job retry / edit a pending job / set handoff output / delete a pipeline |
| `validate_pipeline` / `list_pipeline_templates` | Local spec validation / built-in templates (no daemon) |
| `library_list` | Browse saved spawn presets, saved prompt templates, and built-in pipeline templates in one call (no daemon) |
| `list_schedules` / `create_schedule` / `delete_schedule` | List / create / delete daemon cron/at schedules (see §28) |
| `snapshot_create` / `snapshot_list` / `snapshot_restore` | Worktree+transcript checkpoints & rollback (see §23) |
| `insights` | Mine fleet history for patterns & parallelization wins (see §25) |
| `get_metrics` / `get_pressure` | Live/historical resource metrics / memory-pressure gate (see §11) |
| `savings` | Token-savings ledger (see §29) |
| `search` / `history` / `audit_log` | Full-text search / archived agents / action audit trail |
| `list_plugins` | Registered plugins, their task types & hook events (see §26) |
| `export_sessions` / `import_sessions` | Serialize / load agent session metadata (see §19) |

> All MCP tools are thin wrappers over the same daemon routes (or local helpers)
> the CLI uses, so an orchestrator Claude session can drive a multi-stage workflow
> (analyze→implement→review) — including pause/resume/retry/edit-job and
> scheduling — without shelling out. The complete coverage matrix is in
> [FEATURES.md](../FEATURES.md).

### `/warden` Claude skill

A packaged Claude Code skill teaches any Claude session *how and when* to manage
the fleet (triage, create-from-prompt, relay "tell X to do Y",
terminate-with-confirmation, daemon-down handling, self-rotation). It drives the
MCP tools, falling back to the `warden` CLI when the MCP server isn't registered.

---

## 11. Observability & notifications

| Feature | Description |
|---|---|
| **Resource metrics** | `internal/metrics` collects per-agent process-tree RSS/CPU, system memory/swap/pressure, and daemon self-stats. Exposed via `GET /api/v1/metrics` + `GET /api/v1/metrics/history`. |
| **`warden stats`** | CLI view of the resource metrics. |
| **Metrics recorder** | Optional 15s JSONL recorder (the `metrics` setting). |
| **Agent performance history** | The recorder's samples roll up per agent into runtime, peak/latest/trend RSS, avg/peak CPU, context-token trend, and changed-file count, plus conservative anomaly warnings (climbing memory, climbing/critical context, pinned CPU). Surfaced via `warden stats --history [--agent ID]` and `GET /api/v1/metrics/history?summary=true[&agent=ID]`. |
| **Crash & anomaly detection** | Beyond stuck-state reclassification, the poller flags an **OOM kill** (SIGKILL/exit 137), an **infinite loop** (a churning pane cycling through a few states, distinct from the stuck timer's stale pane), and a **pre-crash context** condition (a critical-but-still-working agent that can't be auto-compacted). Each records a durable `anomaly` event and fires a best-effort notification through the `OnAnomaly` seam (once per episode). |
| **Desktop notifications** | The `notify` setting posts a desktop notification (macOS `osascript` / Linux `notify-send`, log fallback) when an agent needs attention (`waiting_for_input`, stuck `idle`, `orphaned`, `errored`). |
| **Webhook / Slack notifications** | When `webhook_enabled` is on, warden also POSTs a JSON payload to `webhook_url` for every alert that goes to desktop notifications — attention-needed transitions (`waiting_for_input`, `errored`, `orphaned`) and context-size warning/critical alerts. A **Slack incoming-webhook URL works out of the box** (the payload's `text` field is what Slack renders); generic consumers get `{text, title, body}`. Best-effort and non-blocking: a short timeout bounds each POST and failures are logged, never propagated. Runs alongside (not instead of) desktop notifications via the same notifier seam — this is what makes "watch from your phone" push real. |
| **Context-size guard** | `internal/ctxtokens` reads each live agent's context-window fill from its transcript and classifies it `ok`/`warning`/`critical`. The poller shows a state-colored token figure in `ls`/TUI/web, alerts once per upward crossing (`token_warn_alert`), and auto-sends `/compact` at `critical` when the agent is idle (`token_auto_compact`, cooldown-guarded). Master switch `token_guard`; thresholds `token_warn`/`token_critical`. |
| **Structured logging** | `internal/logging` installs a `log/slog` logger (also bridging the standard `log` package) so daemon logs carry structured fields. Level (`log_level`: `debug`/`info`/`warn`/`error`) and format (`log_format`: `text`/`json`) are configurable; `warden daemon --log-level`/`--log-format` override them. |

---

## 12. Configuration (YAML config file)

Settings live in a single YAML file (default `~/.warden/config.yaml`). Run
`warden config init` to generate a fully-commented file, edit values, then restart
the daemon; `warden config` prints what's live. `--config <path>` selects an
alternate file; `--addr <host:port>` overrides the daemon address per-command.

| Setting | Default | Description |
|---|---|---|
| `addr` | `127.0.0.1:8765` | Daemon listen address (loopback only unless `allow_nonloopback`) |
| `data_dir` | `~/.warden` | Warden state directory (sessions, prompts, inbox, pipelines, metrics) |
| `claude_projects_dir` | `~/.claude/projects` | Root of Claude Code transcript dirs (poller reads these) |
| `model_default` | `claude-sonnet-4-6` | Default model for spawned agents (id or alias: `sonnet`/`opus`/`haiku`/`fable`) |
| `default_permission_mode` | `auto` | Default permission mode (valid: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`) |
| `notify` | `false` | Desktop notifications |
| `webhook_enabled` | `false` | POST notifications to `webhook_url` on attention transitions (runs alongside `notify`) |
| `webhook_url` | _(empty)_ | Webhook endpoint; a Slack incoming-webhook URL works out of the box |
| `approvals` | `true` | The approvals inbox |
| `auto_approve` | `false` | Auto-answer recognized yes/no prompts (option 1) |
| `token_guard` | `true` | Context-size guard master switch (gauge + alert + auto-compact) |
| `token_warn_alert` | `true` | Notify once per upward crossing into warning/critical (needs `notify`) |
| `token_auto_compact` | `true` | Auto-`/compact` at `critical` when the agent is idle (cooldown-guarded) |
| `token_warn` | `200000` | Warning threshold in context tokens (resets with critical if critical ≤ warn) |
| `token_critical` | `400000` | Critical threshold in context tokens (auto-`/compact` band) |
| `allow_nonloopback` | `false` | Allow binding the auth-less daemon to a non-loopback address |
| `spawn_gate` / `spawn_gate_max_agents` | `true` / `5` | Soft warning before spawning when many agents are live |
| `budget_gate` / `budget_daily_usd` / `budget_weekly_usd` | `false` / `0` / `0` | Soft warning before spawning when measured Claude spend has reached a $ cap (see §30) |
| `metrics` | `true` | Record per-agent metrics to disk |
| `pipeline_keep_done` / `pipeline_hint` | `false` / `true` | Keep a job's agent after completion / append the decomposition hint |
| `worktree_keep_done` / `worktree_auto_prune` | `false` / `false` | Keep a worktree after its agent is done / auto-reclaim orphaned worktrees |
| `auto_restart_max` / `auto_restart_reset` | `3` / `5m` | Auto-restart attempts for an errored opted-in agent / health window that resets the counter |
| `rate_limit_auto_resume` | `true` | Auto-resume agents after a rate limit clears (`rate_limit_retry_interval`, `rate_limit_buffer` tune timing) |
| `log_level` / `log_format` | `info` / `text` | Daemon log verbosity (`debug`/`info`/`warn`/`error`) and format (`text`/`json`); `warden daemon --log-level`/`--log-format` override |
| `isolation_guard` | `true` | PreToolUse hook blocking an isolated agent from editing files outside its worktree (§22) |
| `git_conventions` | `true` | Append the prompt steer toward `wd commit`/`push`/`sync` over raw git Bash (§22) |
| `git_redirect` | `true` | PreToolUse hook denying raw `git commit`/`push`/`pull`/`rebase`, redirecting to the warden tools (reads stay allowed) (§22) |
| `check_redirect` | `true` | PreToolUse hook redirecting a raw test/lint/build command registered in `.warden/check.yml` to `wd check` (§22) |
| `local_llm` | `false` | Route fuzzy-cheap tasks (classify, summarize, commit messages) to a local Ollama model; falls back to Claude on any error (§21) |
| `local_llm_url` / `local_llm_model` / `local_llm_timeout` | `http://localhost:11434` / `qwen2.5-coder:7b` / `20s` | Local Ollama server URL, model tag, and per-call hard timeout (§21) |
| `local_llm_tier` / `local_llm_escalate` | `auto` / `true` | Orchestrator planning-tier override (`auto`/`t0`/`t1`/`t2`) / allow one over-tier planning step to escalate to headless Claude (§17) |
| `local_llm_classifier` | `heuristic` | How the REPL buckets a request's needed planning tier: `heuristic` (cheap surface signals, no model call) or `model` (a one-shot local-model classification, one extra round-trip, falls back to the heuristic on any error) (§17) |
| `repl` | `false` | Start the cockpit master pane in `wd repl` mode instead of a plain shell (§17) |

> The old `WARDEN_*` environment variables are no longer read — the daemon warns
> once at startup if any are still set. The per-agent IPC vars warden injects
> (`WARDEN_SESSION_ID`, `WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`) are not configuration.

---

## 13. Remote access & authentication

Reach the dashboard and API from a phone, tablet, or any other device — not just
the local machine. Setup recipes (LAN / Tailscale / Cloudflare Tunnel) live in
[USAGE.md](USAGE.md).

| Feature | Description |
|---|---|
| **Bearer-token auth** | A 256-bit `crypto/rand` token gates every non-loopback request (constant-time compare). SSE/WS clients pass it as `?token=`. Binding the daemon to a non-loopback address is **refused unless a token is set**, so remote access can't be opened accidentally. |
| **Token management** | `warden token generate` mints a token, `warden token show` prints the current one (to paste into a remote client), and `warden token rotate` regenerates it in place and restarts the daemon. Persisted to `~/.warden/token.env` (`WARDEN_TOKEN=<hex>`, `0600`); the `WARDEN_TOKEN` env var overrides the file so the secret can stay off disk. |
| **Brute-force protection** | Per-IP rate-limiting on auth failures. |
| **Web UI auth** | A token-entry modal appears on `401`, with `localStorage` persistence and a sign-out control; the static SPA shell stays public so the modal can load. |
| **Mobile-responsive dashboard** | A keyboard-aware layout sized to the visible viewport: a bottom nav that stays pinned above the soft keyboard, single-column grids, full-screen modal sheets, and an interactive terminal you can swipe to scroll (driving tmux/the agent's scrollback like a wheel) with a sticky on-screen key bar (Esc/Tab/Ctrl-C/↑↓/Bottom). |
| **API reference** | Interactive OpenAPI docs at `/api/docs` (raw spec at `/api/docs/openapi.yaml`) document every daemon route and the `bearerAuth` scheme for remote/API consumers — see §27. |

---

## 14. Full-text search

Find agents across a growing fleet without scrolling the grid. The matcher is
in-memory and case-insensitive, AND-ing every whitespace-separated term against a
haystack built from each session's id, name, ticket, type, subject, prompt,
branch, tags, and last-pane excerpt.

| Feature | Description |
|---|---|
| **`warden search <query…>`** | CLI search over active sessions; multiple words are ANDed. `--closed` folds in the archived (`closed/`) store too, `--json` prints raw records. Renders with the same table as `warden ls`. |
| **`GET /api/v1/search?q=&closed=`** | Daemon endpoint (`internal/daemon/search_routes.go`); a blank `q` is a `400`. Returns the standard `{sessions:[…]}` shape. |
| **Web search bar** | The dashboard carries a search box that filters the agent grid live, client-side, mirroring the backend matcher (`web/src/lib/search.ts`) for instant feedback. |

**Tags.** Sessions carry an optional `Tags []string` (`warden start --tags backend,urgent`),
normalized to lowercase and deduped. Tags are part of the search haystack — a bare
`warden search backend` finds every agent labelled `backend`. `warden ls --tag backend
--tag urgent` filters the list to agents carrying *every* given tag (AND semantics, repeatable
or comma-separated). Untagged sessions stay nil and JSON-omit the field, so the change is
backward-compatible with records that predate tags.

---

## 15. Agent history & archive

Browse and search agents that have already ended — warden persists every closed
session to the `closed/` store (newest-first), and this surfaces it.

| Feature | Description |
|---|---|
| **`warden history`** | Lists archived sessions. `--since` accepts a duration (`24h`, `90m`, `7d`, `2w`), a date, or an RFC3339 timestamp; `--type` filters by normalized task type; `--limit` caps the count; `--json` prints raw records. |
| **`GET /api/v1/history?since=&type=&limit=`** | Daemon endpoint (`internal/daemon/history_routes.go`); `since` is RFC3339 (`400` on a bad value), `type` is normalized, `limit` caps the result. |
| **Web Archive tab** | A 🗄 Archive tab fetches history with since (all / 24h / 7d / 30d) and type selectors, plus a client-side text filter, rendering a table of ID / Name / Type / Status / Branch / Updated / Subject. |

---

## 16. Batch operations

Act on many agents at once from the web cockpit instead of one tile at a time.

| Feature | Description |
|---|---|
| **Multi-select** | Per-tile checkboxes on the Cockpit grid, with Shift-click range selection. Selections are pruned automatically when an agent ends. |
| **Bulk action bar** | A floating bar appears while ≥1 agent is selected, offering bulk **Message…**, **Terminate**, and **Delete** (the destructive ones need a second click to confirm). |
| **Sequential fan-out** | Actions reuse the existing per-agent endpoints (`POST /api/v1/sessions/{id}/terminate`/`delete`/`messages`) one at a time (`web/src/lib/batch.ts`); the bar reports partial success and keeps failures selected for retry. (Goroutine-parallel fan-out is parked as #36.) |

---

## 17. Interactive mode / REPL (`wd repl`)

warden's **interactive mode**: a proper terminal REPL to drive the fleet. Run it
standalone (`wd repl`, aliases `wd interactive` / `wd i`) or as
the cockpit master pane (the `repl` config setting / `--repl` flag). It
drives the fleet two ways — a **deterministic `/`-command half** that needs no
model, and a **natural-language half** that turns intent into **confirmed** warden
tool calls without spending Claude tokens. **It conducts; it never implements** —
there is no edit/write/bash/shell tool in its registry, so all code work is
delegated by `spawn_agent`-ing a Claude agent.

It **starts without a local model** — the `/` commands and `!`-shell always work;
only the natural-language half needs `local_llm: true` (a bare line says so when
it's off). This deterministic fallback is why interactive mode no longer hard-fails
without a model.

| Feature | Description |
|---|---|
| **Real line editor** | readline-backed: arrow-key cursor movement, ↑/↓ history persisted to `~/.warden/orch_history`, Ctrl-R reverse-search, Ctrl-A/E/W/K/U editing, a **live `/`-command menu** that filters as you type (each matching verb + its summary, painted under the prompt, Claude-Code style) plus Tab completion of `/` command names **and live agent ids**, **guided argument forms** (pick-lists + free text) when a command needs more input, Ctrl-C to abandon a line, Ctrl-D (or `exit`) to close back to the shell. Prompt/headings are colourised via lipgloss, auto-disabled on non-TTY output and `NO_COLOR`. |
| **Deterministic `/` commands** | Type a `/`-verb (`/agents`, `/spawn <prompt>`, `/tell <id> <text>`, `/stop`, `/commit`/`/push`/`/sync`/`/check`, `/pipelines`, `/ctx*`, `/approvals`, … `/help`) and warden runs the exact verb with **no model in the loop** — reads auto-execute, mutations pass the same confirm gate as the model's calls. An unknown `/verb` is caught with a hint rather than falling through to the model. This is the reliable half: it keeps working when the local model is slow or wrong. |
| **Human-readable read output** | Deterministic read results are rendered for a person, not dumped as raw JSON: `/agents`/`/pipelines` print aligned tables (id · status · type · name · what), `/agent <id>` a tight labelled block (empty fields omitted), `/ctx` a key/value table, and `/inbox`/`/collab`/`/approvals` one compact line each; an unknown shape falls back to indented JSON. The local model still receives the structured JSON for natural-language planning — only the operator-facing `/`-command output is reshaped. |
| **Guided argument forms** | When a `/` command needs more than was typed, warden collects the arguments interactively instead of erroring: a **numbered pick-list** for fields with a known set (`model`, `permission_mode`, `type`, yes/no booleans) and **free text** for open fields. It opens automatically when a required arg is missing (bare `/spawn`), or for **every** field on a `+`-suffixed verb (`/spawn+ <prompt>`). Structure is deterministic (fields + valid options from warden's own enums, so it works model-off); when a local model is present each field opens with a **suggested value** inferred from your words — Enter accepts, type overrides, `-` clears to the config default. The model can only pre-select a *valid* option, never invent one. Shared with the gate's `[e]dit` flow, so the model's NL plans get the same pick-lists. After the form, mutations still pass the confirm gate. |
| **NL → tool-call loop** | Backed by the `internal/llm` `Chatter` seam (Ollama `/api/chat`, multi-turn tool-calling). A bounded turn budget stops runaway loops. The path is hardened against small-model slips: hallucinated args (a fabricated `repo`, a bogus `model`/`type`) are scrubbed before the gate, a required-arg gap is caught and fed back rather than approved into a doomed call, and a malformed tool call is retried as a recoverable hiccup instead of failing the turn. |
| **Read-vs-mutate registry** | Read-only verbs auto-execute (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`, `ctx_list`, `pipeline_list`, `pipeline_get`). The same daemon client the MCP server uses — no new business logic. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`, `pipeline_cancel`, `clean_up`) requires explicit operator approval before it runs — **non-config-gated**, can't be disabled. A batched plan confirms as one unit. |
| **Capability-tier routing** | A cheap pre-classify buckets each request's needed tier against the model's tier (`modelTier`, override with `local_llm_tier`). Classification defaults to a deterministic heuristic; set `local_llm_classifier: model` to swap in a one-shot local-model verdict (falls back to the heuristic on any error) for the hard single-sentence asks the heuristic can't see. Within tier ⇒ plan locally; over tier ⇒ escalate one planning step to headless Claude (`local_llm_escalate`, default on) or degrade honestly — execution always stays token-free warden calls. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize fleet & per-agent state (reusing the `Summarize` routing), `pending_for_me` surfaces what needs the operator, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The REPL takes **no action** on that output — no auto-diagnose/fix/spawn; it reports verbatim and waits. The output is visible as context to the next natural-language turn. A shell that can't start (no PTY) is non-fatal. |
| **Cockpit integration** | As the master pane it hosts `wd repl` over the operator's shell; **Alt+t** toggles the slot to a raw `$SHELL` and back without killing either side (see §8). |
| **Hardware-aware model recommendation** | `wd doctor` best-effort detects accelerator/host memory (NVIDIA VRAM via `nvidia-smi`, Apple unified memory via `sysctl`, else system RAM) and **recommends** a `local_llm_model` sized to fit. It only ever recommends — the operator sets the model; warden never silently swaps it. |
| **`wd llm suggest` — memory-ranked model picker** | A dedicated recommender that auto-detects **two** figures from the *same* memory pool: **total** memory (the bound) and **average free** memory (sampled a few times, smoothing spikes) — VRAM for an NVIDIA GPU, the unified pool on Apple Silicon, else Linux `MemAvailable`. It scores a curated, **tool-calling-forward** catalog (Qwen3, gpt-oss, Mistral Small, Qwen2.5) by **conductor suitability** — not raw size or coding skill, since the REPL routes tool calls and never writes code. Scores are calibrated against the [Berkeley Function-Calling Leaderboard](https://gorilla.cs.berkeley.edu/leaderboard.html) (BFCL v4), weighted toward the multi-turn subcategory the REPL's loop exercises — so a leaner agentic model outranks a bigger coding-tuned one. Each model is marked `fits now` / `free memory first` / `too large`. The ★ pick is the best-scoring model that runs **comfortably now** while leaving headroom for your real workload (Docker, DBs, IDE, Claude sessions, the daemon). `--samples`, `--total-gb`/`--free-gb` overrides, `--json`. |

---

## 18. Audit log (`warden audit log`)

An append-only trail of the daemon's meaningful actions — who did what, when, to
which object — for after-the-fact review. The daemon writes one JSON object per
line to `~/.warden/audit.jsonl` (`internal/audit`), with a stable schema so old
lines stay parseable as fields are added.

| Feature | Description |
|---|---|
| **Recorded actions** | The daemon logs `spawn`, `terminate`, `delete`, `approve`, and pipeline `pipeline_start` / `pipeline_cancel` at the point each succeeds. Each record carries `time` (when), `action` (what), `actor` (who — the request origin), `target` (the agent/pipeline acted on), and an action-specific `detail` map (name, repo, type, option, hard, …). |
| **Best-effort writes** | Recording is fire-and-best-effort: a write failure is logged and swallowed so it never blocks or fails the action being audited. Auditing is on whenever the daemon runs; a nil writer (tests) is a safe no-op. The file is created `0600` — owner-only — since it can name agents and prompts. |
| **`warden audit log`** | Reads and renders the trail (newest last). `--tail N` keeps the most recent N (default 50, `0` = all), `--action` filters by action, `--target` by substring of the agent/pipeline ID, `--since`/`--until` by window (`24h`, `7d`, `2w`) or date, and `--json` prints raw records. It reads the file directly (not via the daemon), so it works even while the daemon is down; malformed/partial lines are skipped. |

---

## 19. Export / import sessions

Serialize session **metadata** to JSON for backup, sharing, or migration between
machines, then read it back into another store. Worktrees, branches, and tmux
sessions are **not** serialized as files and are **not** recreated on import — an
imported record simply remembers where its (now absent) worktree used to live.

| Feature | Description |
|---|---|
| **`warden export`** | Dumps active agent records as a versioned JSON envelope (`{version, exported_at, sessions}`) on stdout. `--all` folds in the archived (`closed/`) store too. Reuses the existing `/sessions` + `/history` reads — no new export endpoint. |
| **`warden import`** | Reads an export envelope from stdin and inserts its records. **Idempotent by id**: a record whose id already exists is skipped, so re-importing the same dump is a no-op. `--merge` overwrites colliding records with the imported data instead; `--json` prints the per-id result. A new id whose name collides with a different record is imported with the alias dropped (reported under `renamed`). |
| **`POST /api/v1/import?merge=`** | Daemon endpoint (`internal/daemon/import_routes.go`); decodes the envelope and inserts each record keyed on id (`400` on a bad body, `422` on a store error). The `store.Export` / `store.ImportResult` envelope types live in `internal/store/portability.go`. |

---

## 20. Docker / container deployment

A multi-stage [`Dockerfile`](../Dockerfile) and [`docker-compose.yml`](../docker-compose.yml)
package the daemon for containerized remote access. The remote-access auth model
(non-loopback bind requires `WARDEN_TOKEN`) carries over unchanged.

| Feature | Description |
|---|---|
| **Multi-stage, lean image** | Stage 1 (`node:22-alpine`) builds the web dashboard; stage 2 (`golang:1.26-alpine`) produces a static `CGO_ENABLED=0` binary with the dashboard `go:embed`-ed in; stage 3 is an `alpine:3.20` runtime carrying only the binary plus `tmux` + `git` + `ca-certificates`. Runs as an unprivileged `warden` user. |
| **Persistent state volume** | `~/.warden` (the session store + config) is a named volume (`/home/warden/.warden`), so records survive container restarts. The import never touches disk worktrees, matching §19 semantics — imported records remember absent worktrees rather than recreating them. |
| **Remote-access defaults** | The entrypoint binds `0.0.0.0:8765`; compose maps the port and threads `WARDEN_TOKEN` from the host environment (required — the daemon refuses a non-loopback bind without it). Front the port with Tailscale / a Cloudflare Tunnel rather than exposing it directly. |
| **tmux/claude boundary** | The image ships `tmux` (hard runtime dependency — every agent runs inside a tmux session) and `git` (worktree-isolated agents). It deliberately omits the `claude` CLI to stay lean: the container hosts the daemon/API/dashboard out of the box; driving live agents additionally needs `claude` + credentials layered in. |

---

## 21. Local-LLM provider (`internal/llm`)

An opt-in, Ollama-compatible local model that handles warden's "fuzzy but cheap"
work without spending Claude tokens. Off by default (`local_llm`); every call has a
hard timeout and a deterministic fallback, so warden behaves exactly as before when
the model is off, unreachable, or wrong. The REPL (§17) is the one surface
that *requires* it; everything below degrades silently to prior behavior.

| Feature | Description |
|---|---|
| **Provider seam** | `internal/llm` exposes one-method `Completer` / `Chatter` interfaces over a small non-streaming Ollama client (`/api/generate`, `/api/chat`) with a hard timeout, response byte cap, and an error-so-the-caller-falls-back contract. The daemon builds the provider only when `local_llm` is on. |
| **Task classification** | `lifecycle.Classify` routes a prompt-spawned agent's type guess through the local model first, falling back to headless Claude, then `other`, on any error. |
| **Activity summaries** | The ≤8-word agent subject (`Summarize`) routes through the same seam, falling back to headless Claude on any error *or empty reply* (an empty summary carries no signal, so unlike `Classify` it is not trusted). |
| **Check-failure condensation** | An **oversized** check-failure log (output past the line cap) is condensed by the local model into its distinct failures; deterministic tail-truncation is the fallback. Within-cap failures skip the model entirely. |
| **Headless commit messages** | `wd commit` / MCP `commit` no longer require `-m`: a missing message is distilled by the local model from the staged diff (`git diff --cached`, capped to 16 KiB) into a Conventional-Commits subject, with a path-derived conventional-commit floor as the guaranteed fallback — a blank commit is impossible. |

Gated by `local_llm` (+ `local_llm_url` / `_model` / `_timeout`); the REPL's
tier knobs (`local_llm_tier` / `_escalate`) live in §17.

---

## 22. Lifecycle commands & boundary enforcement

warden moves deterministic responsibilities off Claude agents — git and checks —
onto first-class commands, and **enforces** the worktree boundary with PreToolUse
hooks: steer via the system prompt → deny-and-redirect the raw escapes. The hooks
are delivered through a per-agent `claude --settings` file; each **fails open** (a
hook error never blocks the agent) and is individually config-gated (default on).
Most of this needs no LLM.

| Feature | Description |
|---|---|
| **`wd commit` / `push` / `sync` (CLI + MCP)** | Git lifecycle on the agent's pinned worktree via the `lifecycle` runner, returning compact structs in place of git tool-spam. Rails: no commit/push on main/master, no dirty-tree sync, pre-commit-hook failure surfaced as a result; `sync` leaves conflicts in progress carrying only the conflicting files. Commit message is auto-filled when `-m` is omitted (§21). |
| **`wd check [name]` (CLI + MCP)** | Runs the per-project `.warden/check.yml` command(s) and returns pass/fail with output for only the **failing** checks (tail-truncated, oversized logs condensed per §21). Per-entry `dir:` for monorepos; config is the single source of truth; no-config / unknown-name return friendly errors. The biggest raw-token win. |
| **Default-isolated write agents** | Every write-type agent (`code`/`docs`/`website`/`debug-ci`/`tests`) gets its own worktree unless `--in-repo`; `pr-review` is exempt (see §2). This is what makes the isolation guard meaningful and fixes parallel-agent collisions. |
| **Isolation guard** (`isolation_guard`) | A PreToolUse hook denies an isolated agent's Edit/Write that escapes its worktree into the shared repo (`warden hook guard` → `POST /api/v1/hooks/guard`). |
| **Git-guard** (`git_redirect`) | A PreToolUse Bash hook quote-aware argv-parses each command and deny-redirects raw `git commit`/`push`/`pull`/`rebase` to the warden tools (reads stay allowed), the deny message naming the exact replacement. Static verdict, no daemon round-trip. |
| **Check-guard** (`check_redirect`) | A PreToolUse Bash hook deny-redirects a raw test/lint/build command the project's `.warden/check.yml` registers to `wd check`, matching on leading-token prefix (broad runs redirect; focused `-run` runs pass through). No-config repos redirect nothing. |
| **Prompt steer** (`git_conventions`) | A Layer-1 system-prompt hint steering agents toward `wd commit`/`push`/`sync` (and `wd check`) over raw git/test Bash — the gentle first layer before the deny hooks. |

---

## 23. Snapshots / checkpoints (`wd snapshot`)

Checkpoint an agent at a known-good point — its **worktree state** *and* its
**session transcript** — and roll back to it later. Config-gated by `snapshots`
(default on); the daemon owns a JSON snapshot store under `<data_dir>/snapshots/`.

- **`wd snapshot create [name] [-m msg]`** (CLI + MCP `snapshot_create`) — captures
  the worktree **non-destructively** via `git stash create` (it builds a commit
  object recording the working tree *without* touching it — no stash entry pushed,
  no index change), recording the HEAD/branch/dirty-file list plus the tmux pane
  scrollback as the transcript. `[name]` defaults to the current agent
  (`WARDEN_SESSION_ID`); the daemon pins capture to that agent's own worktree.
- **`wd snapshot list [name] [--all]`** (CLI + MCP `snapshot_list`) — snapshots
  for an agent (or every session), newest first.
- **`wd snapshot restore <id> [--force]`** (CLI + MCP `snapshot_restore`) —
  re-applies the snapshot's stash onto its recorded worktree. **Rails:** refuses a
  dirty tree unless `--force`, and never restores onto `main`/`master` (same guards
  as `wd sync`/`remove-worktree`). **Reversible-safe** — stash *apply* neither
  resets HEAD nor drops the snapshot, so the snapshot stays usable; a partial apply
  leaves conflicting paths in the tree for resolution (the `wd sync` handoff). The
  saved transcript path is surfaced for the operator regardless.

Built as a self-contained `internal/snapshot` package (pure helpers + a runner
over the shared `lifecycle.Runner` command seam), wired through the daemon →
client → CLI/MCP like the other lifecycle verbs.

## 24. First-run tutorial (`wd tutorial`)

A friendly, idempotent walkthrough of warden's core loop for new operators —
**spawn → watch → talk → tear down** — plus pointers to the cockpit TUI and the
web GUI. It changes nothing: each step shows the exact command to try when you're
ready (`wd start`, `wd ls`/`wd status`, `wd attach`/`wd send`, `wd done`, `wd tui`).

- **`wd tutorial`** — prints the walkthrough, then writes a `tutorial-complete`
  marker in `<data_dir>` so the first-run hint stops appearing.
- **`wd tutorial --skip`** — writes the marker *without* running the steps
  (silences the hint immediately).
- **`wd tutorial --reset`** — deletes the marker so the tour (and the hint) run
  fresh. (`--reset` and `--skip` are mutually exclusive.)

**First-run hint.** On a normal invocation, if the marker is absent, warden prints
a single non-blocking line to **stderr** nudging you toward `wd tutorial`. It is
shown **only** when stdout is an interactive **TTY** *and* the marker is missing
*and* the `tutorial` config setting is on (default on) — and never for piped /
non-TTY output or the machine/full-screen surfaces (daemon, MCP, hooks,
completion, the cockpit root, `wd tui`, the tutorial itself). The walkthrough is
never auto-run and never blocks; automation and the daemon/MCP paths are
untouched. Disable the hint entirely with `tutorial: false` in the config.

Implemented as a thin CLI verb (`internal/cli/tutorial.go`) over pure,
unit-tested helpers — marker read/write/reset, the step list, and the
suppression predicate — with no daemon change.

---

## 25. AI-powered insights (`wd insights`)

Mine warden's **own history** — completed and active agent sessions plus the
resource metrics it already records — into actionable suggestions. Like the
REPL and digest, it is a **deterministic statistics core that needs no
LLM**, with an **optional local-LLM narration layer** that degrades gracefully to
the deterministic text whenever the model is off, unreachable, errors, or returns
an empty reply. Config-gated by `insights` (default on).

- **`wd insights`** (CLI + MCP `insights`) — aggregates history into a report:
  - **session duration by type** — count, median / p90 / max per agent type, with
    individual runs flagged as **outliers** when they exceed 2× the type's median.
  - **parallelization opportunities** — pairs of **finished, same-repo** sessions
    whose run windows did **not** overlap and whose edited file sets are
    **disjoint**, i.e. they could have run concurrently; each carries the wall-clock
    time the shorter run could have saved.
  - **frequently co-edited files** — file pairs touched together across multiple
    sessions, a hint for module coupling.
  - **error rate by type** — errored/orphaned over total per type.
  - **busiest hours (UTC)** — when agent activity clusters.
  - **live agent anomalies** — surfaced straight from the metrics summarizer.
- **Flags** mirror the history/digest neighbors: `--since <24h|7d|2w|date>` to bound
  the window, `--limit` to cap archived sessions mined, `--session <id|name>` to
  scope the parallelization suggestions to one session, and `--json` for the raw
  structured report.

Built as a self-contained, fully unit-tested `internal/insights` package — pure
aggregation (`Analyze`), the parallelization suggester (`SuggestParallelization`),
and the narrator (`Narrate` over the `llm.Completer` seam, a nil completer meaning
deterministic-only) — behind a shared `client.Insights` aggregator that both the CLI
and MCP call. Duration / error-rate / busy-period analysis covers **all** sessions;
the file-set-dependent co-edit and parallelization analysis is strongest on active or
digestible sessions, since archived file sets are reconstructed best-effort from
digests. See
`docs/superpowers/specs/2026-06-25-warden-ai-powered-insights-design.md`.

## 26. Plugin system (`wd plugin`)

Extend warden with **custom agent task types** and **lifecycle hooks** without
forking — a thin, default-off, fail-open extension seam. A plugin is an **external
executable** registered in config and invoked over a documented, versioned
**JSON-over-stdio protocol** (request on stdin, response on stdout, hard timeout),
deliberately mirroring warden's existing PreToolUse guard hooks rather than the
fragile Go `plugin` package or a heavy WASM runtime. Config-gated by `plugins`
(**default off**, since plugins run external code).

- **Lifecycle hooks** — a plugin subscribes to events (`pre-spawn`, `post-spawn`,
  `pre-commit`, `post-commit`, `pre-check`, `post-check`, plus a reserved
  `pre-teardown`); warden invokes it at the spawn/commit/check points with the
  agent's session metadata + event payload. Hooks are **advisory and fail-open**:
  a missing, slow, non-zero-exit, or malformed plugin is logged and skipped, and a
  failing plugin never aborts the others — it can never block, error, or crash an
  agent. A `pre-` hook cannot veto the action (observers, not gates).
- **Custom task types** — a plugin declares new `--type` names, each with its own
  worktree isolation policy. These slot into warden's closed `store.Type` enum via
  a function-var seam so validation/worktree logic recognizes them **without
  changing any built-in type's behavior**; names that collide with a built-in or
  another plugin are rejected at config load.
- **`wd plugin list`** — show registered plugins, their executable paths, declared
  custom task types (with isolation policy), and subscribed hook events; it also
  surfaces any config errors the daemon would reject.

Configure with the `plugins` gate + a `plugin_registry` list (name, path, events,
task_types) in `~/.warden/config.yaml`. Self-contained, fully unit-tested
`internal/plugin` package (`protocol` / `registry` / `dispatcher`). A worked
example lives under `examples/plugins/` (a post-commit notifier). See
`docs/superpowers/specs/2026-06-25-warden-plugin-system-design.md`.

---

## 27. API docs / OpenAPI (`/api/docs`)

A machine-readable **OpenAPI 3.x** description of the daemon's REST API, plus an
interactive **Swagger UI**, so remote/API consumers have a real reference instead
of reading `internal/daemon`. Config-gated by `api_docs` (default on).

| Feature | Description |
|---|---|
| **`GET /api/docs`** | Interactive Swagger UI page. Served from a **pinned, vendored** copy of `swagger-ui-dist@5.17.14` embedded in the binary — **no runtime CDN**, so it works offline and inside the container image. |
| **`GET /api/docs/openapi.yaml`** | The raw OpenAPI document (`application/yaml`). |
| **Spec-first: server generated from the spec** | `openapi.yaml` is the single source of truth; the daemon's typed ("strict") chi server is generated from it with `oapi-codegen` (`internal/daemon/oapi`, via `go generate`). The `*Server` implements the generated `StrictServerInterface`, and ~18 response schemas alias the real Go types (`store.Session`, `lifecycle.*Result`, `snapshot.Snapshot`, `pipeline.Pipeline`, …) via `x-go-type`, so the wire format is byte-identical with zero adapters. |
| **Drift guard (compiler + codegen)** | Every `operationId` becomes an interface method the daemon must implement — an undocumented or mismatched endpoint is a **build failure**, not a stale doc. A CI guard (`make generate-check`) also fails if the committed generated code drifts from the spec. `TestSpecMatchesRoutes` remains as a route-presence smoke test for the hand-registered public/streaming routes outside codegen. |
| **Public surface** | Like `/healthz` and the static SPA shell, the docs are unauthenticated — the spec holds no secrets — while still documenting the `bearerAuth` scheme that gates every data/action route. |

Self-contained `internal/daemon/apidocs` package (`//go:embed` of the spec +
Swagger UI assets), reusing the daemon's existing embed+handler pattern. See
`docs/superpowers/specs/2026-06-25-warden-openapi-api-docs-design.md`.

---

## 28. Native scheduler (`wd schedule`)

Recurring (`--cron`) and single-shot (`--at`) triggers that the daemon fires on
its own timer — no external crontab needed. Each schedule fires **either** one
agent spawn **or** a pipeline, through the same internal seams the `/spawn` and
pipeline routes use (never by shelling out to the CLI). **Opt-in:** gated by
`scheduler_enabled` (default **off**) — the routes return 403 and the reconcile
loop is a no-op until you enable it, because schedules only fire while the daemon
is running. This reverses the original "use OS cron" decision
(`docs/superpowers/specs/2026-06-10-warden-scheduled-pipelines-decision.md`),
keeping the concern as the default-off gate.

| Feature | Description |
|---|---|
| **`wd schedule create <name> --cron "0 9 * * *" --type pr-review --repo <p> --prompt "…"`** | Recurring agent spawn. `--cron` is a 5-field spec (`robfig/cron/v3`, `@daily` etc. supported). |
| **`wd schedule create <name> --at 2026-06-27T09:00 --prompt "…"`** | Single-shot agent spawn — fires once at/after the time, then goes inactive. `--at` is RFC3339 or `2006-01-02T15:04` (local time). |
| **`wd schedule create <name> --cron "…" --pipeline <spec.yaml>`** | Fire a pipeline instead of an agent. Each fire creates a fresh pipeline whose name is timestamp-suffixed, so recurring runs never collide. |
| **`wd schedule list`** | All schedules with kind (cron/at), mode (agent/pipeline), spec, enabled state, next run, and last error. |
| **`wd schedule delete <id>`** | Remove a schedule (id == its name). |
| **No backfill** | On daemon startup each schedule's next-run is recomputed from the wall clock: a cron schedule resumes at its next *future* occurrence (a run missed while the daemon was down is **not** replayed), while a past-due single-shot fires once. |
| **Fail-soft loop** | A fire error is recorded in the schedule's `last_error` and logged; it never crashes the once-a-minute reconcile loop or stops other schedules firing. An agent-name collision fails just that fire (honest over silently renaming). |
| **Read-only MCP + audit** | `list_schedules` (MCP) exposes the same view; create/delete are written to the audit log (`schedule_create` / `schedule_delete`). |

Persisted atomically to `~/.warden/schedules.json`. Pure next-fire logic lives in
`internal/schedule` (table-tested); the daemon's reconcile loop is
`internal/daemon/scheduler.go`.

---

## 29. Token-savings ledger (`wd savings`)

A real, **append-only ledger** of the tokens warden's lifecycle features have kept
out of agents' context windows — a measured proof point, not an estimate. Each time
a feature avoids dumping output into the transcript, the saving is recorded; `wd
savings` reads it back. Config-gated by `savings` (default on); the daemon owns the
store under `<data_dir>/savings/` and serves it at `GET /api/v1/savings` (403 when off).

The report keeps two axes honest and **never blends them into one percentage**:

- **Context axis** — how much leaner agent context stayed (raw output that *would
  have* entered Claude vs. what actually did), reported as a reduction % and dollars.
- **Offload axis** — Claude work moved off entirely onto the local LLM
  (classify/summarize), reported as dollars; it keeps nothing in-context, so it is
  never folded into the context %.

| Feature | Description |
|---|---|
| **`wd savings`** (CLI + MCP `savings`) | Per-feature table sorted biggest-win-first: saved tokens, raw tokens, and event count, under the two headline axes. An empty ledger reads as an explicit "nothing recorded yet". |
| **What records a saving** | `wd check` (raw build/test output kept out of the transcript), `wd commit`/`push`/`sync` (git plumbing output), auto-/`/compact` context reclaim, and local-LLM offload (classify/summarize calls that never hit Claude). |
| **`--benchmark`** | The headline A/B proof: *without warden* (raw tokens that would have entered Claude) vs. *with warden* (what actually did), the reduction %, the leaner factor, dollars saved, a per-day **saved-tokens sparkline**, and — when transcript spend was observed — the cut as a share of real measured Claude spend. Built to screenshot. |
| **`--since <window\|date>`** | Scope to a window (`24h`, `7d`, `2w`) or a date. |
| **`--json`** | Structured summary for tooling. |
| **`--audit`** | Print a few retained **raw-vs-kept provenance samples** so a skeptic can eyeball the actual bytes behind the counts. Requires `savings_samples` (off by default — samples retain substrings of real build/test/git output, which may be sensitive). |
| **`--calibrate`** | Measure this workload's true **bytes-per-token** ratio against Claude's `count_tokens` endpoint (needs `ANTHROPIC_API_KEY` + retained samples) and persist it, so figures stop relying on the generic 4-bytes/token heuristic. **Forward-only:** it prices events recorded after calibration; earlier rows keep their heuristic counts. `--calibrate-max` caps the paid calls. |

Every figure states its **basis** — `CALIBRATED` (workload-measured) or `HEURISTIC`
(4 bytes/token) — so the claim is never ambiguous, and dollars are priced at the
Opus input/output rates. Self-contained, fully unit-tested `internal/savings`
package (`store` / `savings` / `calibrate`); the CLI rendering lives in
`internal/cli/savings.go`.

## 30. Cost governance (`wd spend` + budget gate)

The cost counterpart to the savings ledger: where `wd savings` reports what warden
kept **out** of context, `wd spend` reports what agents **actually billed** Claude.
The daemon already reads each agent's REAL input/output tokens from its transcript
(`internal/spend`); cost governance prices that per model into dollars and gates on
it. Config-gated by `savings` (the same switch — spend is the cost half of the same
measured data); the daemon serves it at `GET /api/v1/spend` (403 when off).

| Feature | Description |
|---|---|
| **`wd spend`** (CLI + MCP `spend`) | The measured spend priced per model and rolled up three ways — **per agent**, **per repo**, **per day** — with a headline `total / today / this week`. `--by agent\|repo\|day` shows one rollup; `--json` for tooling. An empty meter reads as "nothing measured yet". |
| **Per-model pricing** | A small `$/Mtok` table (`internal/spend/pricing.go`): Opus `$5/$25`, Sonnet `$3/$15`, Haiku `$0.8/$4` (in/out); an unrecognized model is priced at the Opus tier so spend is never silently under-counted. Opus rates are kept in sync with the savings ledger. |
| **Budget gate** | A **soft** spawn gate, sibling to the memory-pressure gate: when today's or the trailing-week's measured spend reaches a configured cap, a non-forced spawn returns `428` with a confirmation payload; re-run with `--force` to proceed. Off by default. Tunable via `budget_gate` / `budget_daily_usd` / `budget_weekly_usd` (a `0` cap disables that axis). |
| **`$` in `wd ls`** | A **COST** column shows each agent's measured spend beside its context fill (best-effort — `—` when the feature is off). Also surfaced in `wd search` / `wd history`. |
| **Web Metrics tab** | A **Cost per agent** card: the `total / today / this week` headline plus a live per-agent cost table beside the RSS/CPU charts. |

Self-contained, fully unit-tested `internal/spend` package (`store` / `pricing` /
`report` / `budget`); the CLI rendering lives in `internal/cli/spend.go`.
