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
| `terminate` | Stop an agent (kill tmux + claude); **keeps** the record and worktree. The safe, reversible "stop" default. |
| `restore` | Recreate and resume a lost/orphaned agent's session (`claude --resume`). |
| `done` | Terminate **and** clear the record in one step (worktree kept). `--hard` purges instead of archiving. `--create-pr` first pushes the agent's branch and opens a GitHub PR (`gh`) titled from the agent and bodied from its digest (`--base` sets the target, default main) — the PR is opened *before* termination, so a failure leaves the agent running to retry; an existing PR for the branch is reported, not re-created. |
| `delete` | Clear the stored record (archive by default, `--hard` purge). Leaves tmux + worktree alone. |
| `remove-worktree` | Remove the git worktree + branch. **Destructive** — refuses while the agent runs or has uncommitted/unpushed work unless `--force`. |
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
| **File-conflict detection** (`warden collab`) | The daemon watches each active agent's worktree and warns (via the inbox, deduplicated) when two agents are editing the same file. Detection is **real-time**: an fsnotify watcher reacts to edits in subseconds (a burst of saves is debounced into one scan), while a slower `git diff` poll reconciles the watch set against the active-session view and acts as a safety net — degrading cleanly to pure polling if fsnotify is unavailable or the inotify watch budget (80% of the per-user limit) is exhausted. Inspect with `collab conflicts` / `collab who-is-editing <file>`, `GET /collab/conflicts`, the `get_collaboration_status` / `who_is_editing_file` MCP tools, or the **File conflicts** card on the dashboard Overview. Spawned agents also get a system-prompt hint to check `who_is_editing_file` and their inbox before editing shared files, so they coordinate rather than overwrite. Tunable via `collab_enabled` / `collab_interval` / `collab_hint`. |

---

## 7. Self-rotation (`warden rotate`)

Run **inside an agent session** to retire a long-lived, context-heavy agent and
hand off to a fresh successor in the same workdir/worktree. Phase 1 (writing the
handoff file + resume prompt) is driven by the `/warden` skill; on confirmation
the agent spawns its successor and reaps itself.

- **Spawn-before-reap** is fail-safe — if the successor fails to spawn, the
  current agent keeps running.
- Rotation **reuses the worktree by cwd and never removes it** (a compile-time
  invariant: the rotator interface omits worktree removal).

**Cross-agent handoff (`warden handoff`)** is the delegation counterpart: an
agent (or the operator) hands a context package to a **different** agent and
**keeps running**. Default mode spawns a fresh delegate in its **own isolated
worktree**; `--to <id>` delivers the handoff into an existing agent's inbox
(waking it). The handoff file's **content is inlined** into the recipient's
prompt/message — the recipient runs in a different worktree and can't read the
source's file by path. Phase 1 (writing the handoff + resume prompt) is
skill-driven; the source agent is **never** terminated (a compile-time invariant:
the handoff interface omits termination).

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
| **Tabbed mission-control shell** | Fixed **Overview** and **Cockpit** tabs, plus one closeable tab per pinned agent. |
| **Live fleet over SSE** | No manual refresh; coloured busy/idle badges (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) + each agent's subject. |
| **Attention queue** | Surfaces agents in `waiting_for_input`/`errored`/`orphaned`, with one-click approval buttons. |
| **Cockpit tab** | Multi-pane view for watching several agents at once. |
| **Agent grouping** | The Cockpit grid buckets agents into collapsible panes by **Directory / Type / Status / Tag** (multi-tagged agents appear under each tag; untagged agents bucket together). The choice is saved to LocalStorage; the Overview mini-grid stays grouped by directory. |
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. |
| **Create agent** | **+ New agent** prompt box with a directory picker (live prefix autocomplete) and a **Supervised** checkbox. |
| **Terminate with git guard** | Surfaces a 409 → **Force** + optional hard-delete when there's uncommitted/unpushed work. |
| **Digest panel** | View an agent's completion digest in the browser. |
| **Resources panel** | Live per-agent + system resource charts (uPlot). |
| **Activity timeline** | Event timeline / activity feed of fleet state changes (`EventTimeline`, `ActivityFeed`). |
| **Pipeline DAG view** | Pipelines rendered as their dependency graph (`PipelineDag`). |
| **Browser notifications** | Opt-in desktop notification when an agent enters `waiting_for_input` (gated to hidden tabs). |
| **Theme toggle (light/dark/system)** | Header toggle cycles **System → Light → Dark**, defaulting to System (follows the OS via `prefers-color-scheme`). The choice persists in `localStorage` (`warden.theme`) and is applied before first paint to avoid a flash. The palette is themed through CSS custom properties keyed on `<html data-theme>`, which also pins `color-scheme` so system colors flip on an explicit override; the wordmark swaps to match the resolved theme. |
| **Keyboard shortcuts** | Global shortcut layer: `?` toggles a help overlay (also reachable from the header `?` button), `n` new agent, `/` focus the agent filter, `r` refresh the fleet, `1`–`9` jump to a tab, `j`/`k` next/previous tab, `Esc` close a modal/overlay or blur a field. Single-key bindings stay dormant while typing in a field (Esc still fires). |

---

## 10. Orchestration (MCP)

`warden mcp` is a stdio MCP server so an orchestrator Claude session can manage
the fleet through tool calls. Tools exposed:

| Tool | Purpose |
|---|---|
| `list_agents` / `get_agent` | List agents / full detail for one |
| `spawn_agent` | Spawn (prompt mode or `type`+`repo`; `supervised` opt-in) |
| `adopt_agent` | Register an existing Claude session |
| `send_to_agent` / `get_agent_output` | Type into / read recent output of an agent |
| `terminate_agent` / `restore_agent` | Stop (reversible) / resume an agent |
| `delete_agent` / `remove_worktree` | Clear record / remove worktree (guarded) |
| `ctx_set` / `ctx_get` / `ctx_list` | Shared-context blackboard |
| `send_message` / `read_inbox` | Directed messaging |
| `list_approvals` / `approve` | List / answer pending tool-permission prompts |
| `create_pipeline` / `list_pipelines` / `show_pipeline` | Create a DAG pipeline from a YAML spec / list / inspect (jobs, branches, handoffs) |
| `start_pipeline` / `cancel_pipeline` | Start (spawn entry jobs) / cancel (terminate live jobs) a pipeline |

> Pipeline MCP tools are thin wrappers over the same daemon routes the CLI uses,
> so an orchestrator Claude session can drive a multi-stage workflow
> (analyze→implement→review) without shelling out. Pause/resume/delete/edit-job/retry
> remain CLI-only (`warden pipeline …`).

### `/warden` Claude skill

A packaged Claude Code skill teaches any Claude session *how and when* to manage
the fleet (triage, create-from-prompt, relay "tell X to do Y",
terminate-with-confirmation, daemon-down handling, self-rotation). It drives the
MCP tools, falling back to the `warden` CLI when the MCP server isn't registered.

---

## 11. Observability & notifications

| Feature | Description |
|---|---|
| **Resource metrics** | `internal/metrics` collects per-agent process-tree RSS/CPU, system memory/swap/pressure, and daemon self-stats. Exposed via `/metrics` + `/metrics/history`. |
| **`warden stats`** | CLI view of the resource metrics. |
| **Metrics recorder** | Optional 15s JSONL recorder (the `metrics` setting). |
| **Agent performance history** | The recorder's samples roll up per agent into runtime, peak/latest/trend RSS, avg/peak CPU, context-token trend, and changed-file count, plus conservative anomaly warnings (climbing memory, climbing/critical context, pinned CPU). Surfaced via `warden stats --history [--agent ID]` and `GET /metrics/history?summary=true[&agent=ID]`. |
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
| `metrics` | `true` | Record per-agent metrics to disk |
| `pipeline_keep_done` / `pipeline_hint` | `false` / `true` | Keep a job's agent after completion / append the decomposition hint |
| `worktree_keep_done` / `worktree_auto_prune` | `false` / `false` | Keep a worktree after its agent is done / auto-reclaim orphaned worktrees |
| `auto_restart_max` / `auto_restart_reset` | `3` / `5m` | Auto-restart attempts for an errored opted-in agent / health window that resets the counter |
| `rate_limit_auto_resume` | `true` | Auto-resume agents after a rate limit clears (`rate_limit_retry_interval`, `rate_limit_buffer` tune timing) |
| `log_level` / `log_format` | `info` / `text` | Daemon log verbosity (`debug`/`info`/`warn`/`error`) and format (`text`/`json`); `warden daemon --log-level`/`--log-format` override |

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
| **Mobile-responsive dashboard** | Bottom nav, single-column grids, and full-screen modal sheets so the GUI is usable on a phone. |

---

## 14. Full-text search

Find agents across a growing fleet without scrolling the grid. The matcher is
in-memory and case-insensitive, AND-ing every whitespace-separated term against a
haystack built from each session's id, name, ticket, type, subject, prompt,
branch, tags, and last-pane excerpt.

| Feature | Description |
|---|---|
| **`warden search <query…>`** | CLI search over active sessions; multiple words are ANDed. `--closed` folds in the archived (`closed/`) store too, `--json` prints raw records. Renders with the same table as `warden ls`. |
| **`GET /search?q=&closed=`** | Daemon endpoint (`internal/daemon/search_routes.go`); a blank `q` is a `400`. Returns the standard `{sessions:[…]}` shape. |
| **Web search bar** | The Overview tab carries a search box that filters the All-agents grid live, client-side, mirroring the backend matcher (`web/src/lib/search.ts`) for instant feedback. |

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
| **`GET /history?since=&type=&limit=`** | Daemon endpoint (`internal/daemon/history_routes.go`); `since` is RFC3339 (`400` on a bad value), `type` is normalized, `limit` caps the result. |
| **Web Archive tab** | A 🗄 Archive tab fetches history with since (all / 24h / 7d / 30d) and type selectors, plus a client-side text filter, rendering a table of ID / Name / Type / Status / Branch / Updated / Subject. |

---

## 16. Batch operations

Act on many agents at once from the web cockpit instead of one tile at a time.

| Feature | Description |
|---|---|
| **Multi-select** | Per-tile checkboxes on the Cockpit grid, with Shift-click range selection. Selections are pruned automatically when an agent ends. |
| **Bulk action bar** | A floating bar appears while ≥1 agent is selected, offering bulk **Message…**, **Terminate**, and **Delete** (the destructive ones need a second click to confirm). |
| **Sequential fan-out** | Actions reuse the existing per-agent endpoints (`POST /sessions/{id}/terminate`/`delete`/`messages`) one at a time (`web/src/lib/batch.ts`); the bar reports partial success and keeps failures selected for retry. (Goroutine-parallel fan-out is parked as #36.) |

---

## 17. Orchestrator (`wd orch`)

A warden-aware, **local-LLM** conductor REPL that turns natural-language operator
intent into **confirmed** warden tool calls — spawn/monitor/teardown agents, drive
pipelines, run the git/check lifecycle — without spending Claude tokens. Run it
standalone (`wd orch`, alias `wd orchestrator`) or as the cockpit master pane (the
`orchestrator` config setting / `--orch` flag). Requires `local_llm: true`; it's an
interactive surface with no deterministic fallback. **It conducts; it never
implements** — there is no edit/write/bash/shell tool in its registry, so all code
work is delegated by `spawn_agent`-ing a Claude agent.

| Feature | Description |
|---|---|
| **NL → tool-call loop** | Backed by the `internal/llm` `Chatter` seam (Ollama `/api/chat`, multi-turn tool-calling). A bounded turn budget stops runaway loops; malformed args / unknown tools recover instead of garbling execution. |
| **Read-vs-mutate registry** | Read-only verbs auto-execute (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`, `ctx_list`, `pipeline_list`, `pipeline_get`). The same daemon client the MCP server uses — no new business logic. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`, `pipeline_cancel`, `clean_up`) requires explicit operator approval before it runs — **non-config-gated**, can't be disabled. A batched plan confirms as one unit. |
| **Capability-tier routing** | A cheap T0 pre-classify buckets each request's needed tier against the model's tier (`modelTier`, override with `local_llm_tier`). Within tier ⇒ plan locally; over tier ⇒ escalate one planning step to headless Claude (`local_llm_escalate`, default on) or degrade honestly — execution always stays token-free warden calls. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize fleet & per-agent state (reusing the `Summarize` routing), `pending_for_me` surfaces what needs the operator, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The orchestrator takes **no action** on that output — no auto-diagnose/fix/spawn; it reports verbatim and waits. The output is visible as context to the next natural-language turn. A shell that can't start (no PTY) is non-fatal. |
| **Cockpit integration** | As the master pane it hosts `wd orch` over the operator's shell; **Alt+t** toggles the slot to a raw `$SHELL` and back without killing either side (see §8). |
| **Hardware-aware model recommendation** | `wd doctor` best-effort detects accelerator/host memory (NVIDIA VRAM via `nvidia-smi`, Apple unified memory via `sysctl`, else system RAM) and **recommends** a `local_llm_model` from the Qwen2.5-Coder family sized to fit (≥20 GB → `32b` · ~10 → `14b` · ~6 → `7b` · ~4 → `3b` · ≤2 → `1.5b`). It only ever recommends — the operator sets the model; warden never silently swaps it. |

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
| **`POST /import?merge=`** | Daemon endpoint (`internal/daemon/import_routes.go`); decodes the envelope and inserts each record keyed on id (`400` on a bad body, `422` on a store error). The `store.Export` / `store.ImportResult` envelope types live in `internal/store/portability.go`. |

---

## 19. Docker / container deployment

A multi-stage [`Dockerfile`](../Dockerfile) and [`docker-compose.yml`](../docker-compose.yml)
package the daemon for containerized remote access. The remote-access auth model
(non-loopback bind requires `WARDEN_TOKEN`) carries over unchanged.

| Feature | Description |
|---|---|
| **Multi-stage, lean image** | Stage 1 (`node:22-alpine`) builds the web dashboard; stage 2 (`golang:1.26-alpine`) produces a static `CGO_ENABLED=0` binary with the dashboard `go:embed`-ed in; stage 3 is an `alpine:3.20` runtime carrying only the binary plus `tmux` + `git` + `ca-certificates`. Runs as an unprivileged `warden` user. |
| **Persistent state volume** | `~/.warden` (the session store + config) is a named volume (`/home/warden/.warden`), so records survive container restarts. The import never touches disk worktrees, matching §18 semantics — imported records remember absent worktrees rather than recreating them. |
| **Remote-access defaults** | The entrypoint binds `0.0.0.0:8765`; compose maps the port and threads `WARDEN_TOKEN` from the host environment (required — the daemon refuses a non-loopback bind without it). Front the port with Tailscale / a Cloudflare Tunnel rather than exposing it directly. |
| **tmux/claude boundary** | The image ships `tmux` (hard runtime dependency — every agent runs inside a tmux session) and `git` (worktree-isolated agents). It deliberately omits the `claude` CLI to stay lean: the container hosts the daemon/API/dashboard out of the box; driving live agents additionally needs `claude` + credentials layered in. |
