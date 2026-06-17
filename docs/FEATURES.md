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
| **Security hardening** | `0700` data dir, slowloris/body/write timeouts (bypassed for SSE/WS/long-poll), refuses non-loopback bind unless `WARDEN_ALLOW_NONLOOPBACK=1`. |
| **`warden doctor`** | Preflight checks: required binaries (`tmux`, `git`, `claude`, `gh`), daemon reachability, data directory. |

---

## 2. Spawning agents

| Feature | Description |
|---|---|
| **Prompt-spawn** | `warden start "<prompt>"` — no repo or type needed. Runs `claude` in the caller's directory (or `--dir`). |
| **Auto-classification** | The daemon classifies a prompt-spawned agent's type with `claude -p` shortly after creation (falls back to `other`). |
| **Auto-generated subject** | Each agent gets a one-line ≤8-word summary of what it's doing, seeded from the prompt and refreshed by the poller from the transcript or tmux pane (throttled, change-gated). |
| **Managed worktree spawn** | `--type` creates/adopts a git worktree where the type needs one. |
| **Worktree adoption** | If a worktree for the ticket already exists, the spawn reattaches to it instead of erroring. |
| **Configurable permission mode** | Per-agent and global control over Claude permission level. CLI flag: `--permission-mode <mode>` (values: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`). Legacy alias: `--supervised` (equivalent to `--permission-mode acceptEdits`). Global default: `WARDEN_DEFAULT_PERMISSION_MODE` env var (defaults to `auto`). Runtime change: `warden set-permission-mode <id> <mode>`. Display: PERMISSION_MODE column in `warden ls`, permission_mode field in `warden status`. Stored in session: mode preserved on restore/resume. Empty mode means "use global default" and displays as `default`. |
| **Model selection** | Per-agent model selection via `--model` flag (CLI and MCP). Short aliases for common models: `opus`, `sonnet`, `haiku`, `fable`. Environment variable default: `WARDEN_MODEL_DEFAULT`. Fallback: `claude-sonnet-4-5` if not specified. Display: MODEL column in `warden ls`, model field in `warden status`. Stored in session: model preserved on restore/resume. |

### Task types (`--type`)

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | `.worktrees/<ticket>` on a branch named after the ticket |
| `pr-review` | yes (PR branch) | Detached worktree; runs `gh pr checkout <PR>`. Needs `--pr`/`--branch` |
| `analysis` | opt-in (`--worktree`) | Runs in the repo by default |
| `spike` | opt-in (`--worktree`) | Same as analysis |
| `code` | no | Runs in the repo root |
| `docs` | no | Runs in the repo root |
| `website` | no | Runs in the repo root |
| `debug-ci` | no | Runs in the repo root |
| `tests` | no | Runs in the repo root |
| `other` | no | Catch-all / unrecognized type strings |

---

## 3. Lifecycle management

| Command / feature | Description |
|---|---|
| `terminate` | Stop an agent (kill tmux + claude); **keeps** the record and worktree. The safe, reversible "stop" default. |
| `restore` | Recreate and resume a lost/orphaned agent's session (`claude --resume`). |
| `done` | Terminate **and** clear the record in one step (worktree kept). `--hard` purges instead of archiving. |
| `delete` | Clear the stored record (archive by default, `--hard` purge). Leaves tmux + worktree alone. |
| `remove-worktree` | Remove the git worktree + branch. **Destructive** — refuses while the agent runs or has uncommitted/unpushed work unless `--force`. |
| `adopt` | Register an existing Claude session — resume newest-for-dir under tmux, or live-register a running tmux session. |
| **Cascade cleanup** | Deleting a pipeline/agent cascades cleanup of its shared-context keys and (on hard-delete) its mailbox inbox. |

---

## 4. Observation & interaction

| Command / feature | Description |
|---|---|
| `ls` | List active agents (type, status, age, dir, subject). `--json` for machine output. |
| `status <id>` | Full detail for one agent — workdir, subject, worktree, branch, PR, events. `--json` available. |
| `attach <id>` | Attach your terminal to the agent's tmux session interactively. |
| `send <id> <msg>` | Type a message into the agent's claude session and press Enter. |
| `tail <id>` | Print recent terminal output (`--lines N`). |
| `digest <id>` | Completion digest — files touched, branch, turn count, and a best-effort `claude -p` narrative. `--json` available. |
| **Stuck / attention detection** | Agents flagged `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`, surfaced across all interfaces. |

---

## 5. Approvals inbox

Answer routine Claude tool-permission prompts (from supervised agents) without
attaching. Controlled by `WARDEN_APPROVALS` (on by default).

| Surface | How |
|---|---|
| **CLI** | `warden approvals` lists recognized pending prompts with their numbered options; `warden approve <id> <n>` answers one. |
| **Web** | One-click option buttons in the AttentionQueue. |
| **TUI** | A pinned **⏳ Approvals** row (`i` / `enter`, then `1`-`9`; `tab` cycles agents). |
| **Safety** | A TOCTOU re-capture + fingerprint re-verify guards answers; unrecognized prompts always fall back to attach. |

### Auto-Approve

Automatically approve yes/no tool-permission prompts by always selecting option 1.
Off by default (opt-in safety), enabled globally via `WARDEN_AUTO_APPROVE` or
per-agent via `warden auto-approve <id> on|off`.

**Behavior:**
- Only triggers for recognized yes/no prompts (parsed via `approval.Parse`)
- Always selects option 1 (predictable, auditable behavior)
- Skips multi-select, text-entry, and unrecognized prompts (falls back to manual approval)
- Logs all auto-approval attempts (success/skip/failure) for auditing
- Per-agent setting overrides global default

**Configuration:**
```bash
# Enable globally (all supervised agents)
export WARDEN_AUTO_APPROVE=on
warden daemon

# Toggle for specific agent
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
| **Pipelines** (`warden pipeline`) | YAML-defined **DAG of dependent agent jobs**. The daemon runs them: dependency-free jobs start first, each job's `emit` publishes output and unblocks dependents — keeping the lead Claude off the critical path. Sub-commands: `create`, `start`, `show`, `list`, `retry`, `cancel`, `delete`. CLI-only (no MCP tools yet), with full TUI + web visibility. |
| **Shared context** (`warden ctx`) | A namespaced key/value blackboard all agents can read/write: `ctx set`/`get`/`list`. |
| **Directed messages** (`warden msg`) | Per-agent inbox: `msg send` (wakes a parked idle/waiting agent), `msg inbox`, `msg wait` (blocks in the daemon until a message arrives). |

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
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. |
| **Create agent** | **+ New agent** prompt box with a directory picker (live prefix autocomplete) and a **Supervised** checkbox. |
| **Terminate with git guard** | Surfaces a 409 → **Force** + optional hard-delete when there's uncommitted/unpushed work. |
| **Digest panel** | View an agent's completion digest in the browser. |
| **Resources panel** | Live per-agent + system resource charts (uPlot). |
| **Browser notifications** | Opt-in desktop notification when an agent enters `waiting_for_input` (gated to hidden tabs). |

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

> Pipelines are CLI-only — no MCP pipeline tools yet.

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
| **Metrics recorder** | Optional 15s JSONL recorder (`WARDEN_METRICS`). |
| **macOS notifications** | `WARDEN_NOTIFY=on` posts a desktop notification when an agent needs attention (`waiting_for_input`, stuck `idle`, `orphaned`, `errored`). |
| **Context-size guard** | `internal/ctxtokens` reads each live agent's context-window fill from its transcript and classifies it `ok`/`warning`/`critical`. The poller shows a state-colored token figure in `ls`/TUI/web, alerts once per upward crossing (`WARDEN_TOKEN_WARN_ALERT`), and auto-sends `/compact` at `critical` when the agent is idle (`WARDEN_TOKEN_AUTO_COMPACT`, cooldown-guarded). Master switch `WARDEN_TOKEN_GUARD`; thresholds `WARDEN_TOKEN_WARN`/`WARDEN_TOKEN_CRITICAL`. |

---

## 12. Configuration (environment variables)

| Variable | Default | Description |
|---|---|---|
| `WARDEN_ADDR` | `127.0.0.1:8765` | Daemon listen address |
| `WARDEN_DATA_DIR` | `~/.warden` | Session JSON store directory |
| `WARDEN_WORKDIR` | `~/warden-agents` | Where per-agent prompt files are stored |
| `CLAUDE_PROJECTS_DIR` | `~/.claude/projects` | Root of Claude Code transcript dirs (poller reads these) |
| `WARDEN_DEFAULT_PERMISSION_MODE` | `auto` | Default permission mode for spawned agents (valid: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`) |
| `WARDEN_NOTIFY` | `off` | macOS desktop notifications |
| `WARDEN_APPROVALS` | `on` | The approvals inbox |
| `WARDEN_TOKEN_GUARD` | `on` | Context-size guard master switch (gauge + alert + auto-compact) |
| `WARDEN_TOKEN_WARN_ALERT` | `on` | Notify once per upward crossing into warning/critical (needs `WARDEN_NOTIFY`) |
| `WARDEN_TOKEN_AUTO_COMPACT` | `on` | Auto-`/compact` at `critical` when the agent is idle (cooldown-guarded) |
| `WARDEN_TOKEN_WARN` | `200000` | Warning threshold in context tokens (resets with critical if critical ≤ warn) |
| `WARDEN_TOKEN_CRITICAL` | `400000` | Critical threshold in context tokens (auto-`/compact` band) |
| `WARDEN_ALLOW_NONLOOPBACK` | unset | Allow binding a non-loopback address |

All can be overridden with `--addr` on any command.
