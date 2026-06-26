# warden — operating the fleet (savings, insights, audit, scheduler, config, remote, UIs)

The cross-cutting capabilities that run the fleet rather than a single agent.

## Token-savings ledger — `savings` / `wd savings`

A real, **append-only ledger** of the tokens warden's lifecycle features have kept
out of agents' context windows — a measured proof point, not an estimate. This is
the concrete payoff of preferring warden's `check`/`commit`/`push`/`sync` over raw
Bash: each time a feature avoids dumping output into the transcript, the saving is
recorded. Config-gated by `savings` (default on); served at `GET /api/v1/savings`.
MCP `savings {since?, bucket?, samples?}` returns the structured Summary; the CLI
adds the human-readable table and `--benchmark` headline.

- `wd savings` — per-feature table (saved tokens, raw tokens, event count), kept on
  **two axes that are never blended into one number**: a **context** axis (how much
  leaner agent context stayed, as a reduction % and dollars) and an **offload** axis
  (Claude work moved entirely onto the local LLM, dollars only).
- What records a saving: `wd check` (raw build/test output), `wd commit`/`push`/`sync`
  (git plumbing output), auto-/`/compact` context reclaim, and local-LLM offload.
- `--benchmark` — the screenshot-ready A/B headline: *without warden* vs *with
  warden* tokens, the reduction %, leaner factor, dollars saved, a per-day
  sparkline, and the cut as a share of real measured Claude spend when observed.
- `--since <window|date>`, `--json`. `--audit` prints retained raw-vs-kept
  provenance samples (needs `savings_samples`, off by default — samples may be
  sensitive). `--calibrate` measures this workload's true bytes-per-token against
  Claude's `count_tokens` (needs `ANTHROPIC_API_KEY` + samples) so figures stop
  relying on the 4-bytes/token heuristic; every figure states its basis
  (`CALIBRATED` vs `HEURISTIC`).

Reach for this when the user asks "how much is warden saving me" or wants proof the
lifecycle tools pay off.

## Insights — mine warden's own history

## Insights — mine warden's own history

MCP `insights {limit?}`; CLI `wd insights`. A **deterministic** report (no LLM
needed; optional local-LLM narration). Config-gated by `insights` (default on).

- **session duration by type** — count, median / p90 / max, with runs flagged
  **outlier** at >2× the type's median.
- **parallelization opportunities** (the headline) — pairs of finished, same-repo
  sessions that ran sequentially but touched **disjoint** files, i.e. could have
  run concurrently as a 2-job pipeline; each carries the wall-clock saving.
- **frequently co-edited files**, **error rate by type**, **busiest hours (UTC)**,
  **live-agent anomalies**.
- CLI flags: `--since <24h|7d|2w|date>`, `--limit`, `--session <id|name>`, `--json`.

Reach for this whenever the user asks "what could've been parallel" or "how's the
fleet doing" — don't eyeball it.

## Audit log — `audit_log` / `wd audit log`

MCP `audit_log {action?, target?, since?, until?, limit?}` returns the events;
`warden audit log` is the CLI view — an append-only trail of meaningful daemon actions (`spawn`,
`terminate`, `delete`, `approve`, `pipeline_start`/`pipeline_cancel`,
`schedule_create`/`schedule_delete`) at `~/.warden/audit.jsonl` (`0600`). Each
record: `time`, `action`, `actor`, `target`, action-specific `detail`. Flags:
`--tail N` (default 50, `0`=all), `--action`, `--target`, `--since`/`--until`,
`--json`. Reads the file directly, so it works even while the daemon is down.

## Scheduler — recurring / single-shot triggers

Daemon-fired triggers — no external crontab. **Opt-in:** gated by
`scheduler_enabled` (default **off**); routes return 403 and the loop is a no-op
until enabled (schedules only fire while the daemon runs). Full MCP coverage:
`list_schedules` (read-only), `create_schedule {name, cron|at, type/repo/prompt | spec}`,
`delete_schedule {id}` (all 403 when disabled).

```sh
warden schedule create <name> --cron "0 9 * * *" --type pr-review --repo <p> --prompt "…"
warden schedule create <name> --at 2026-06-27T09:00 --prompt "…"        # single-shot
warden schedule create <name> --cron "…" --pipeline <spec.yaml>          # fire a pipeline
warden schedule list      # kind (cron/at), mode (agent/pipeline), spec, enabled, next run, last error
warden schedule delete <id>
```

`--cron` is a 5-field spec (`robfig/cron/v3`, `@daily` etc.); `--at` is RFC3339 or
`2006-01-02T15:04` (local). A pipeline fire gets a timestamp-suffixed name so
recurring runs never collide. **No backfill** — a run missed while the daemon was
down is not replayed. Fail-soft: a fire error is recorded in `last_error`, never
crashes the loop.

Prefer this over OS cron or a separate scheduler skill — it fires through the same
internal seams the spawn/pipeline routes use, and it's audited.

## Configuration (CLI-only)

A single YAML file (default `~/.warden/config.yaml`). `warden config init`
generates a fully-commented file; edit, then restart the daemon. `warden config`
prints what's live (there is no `config set` subcommand — edit the YAML by hand;
`wd config path` locates it). `--config <path>` selects an alternate file;
`--addr <host:port>` overrides the daemon address per-command.

Notable settings (see the generated file for the full set with defaults):

- **Models / permissions:** `model_default`, `default_permission_mode`.
- **Notifications:** `notify`, `webhook_enabled`/`webhook_url` (a Slack
  incoming-webhook URL works out of the box), browser notifications.
- **Token guard:** `token_guard`, `token_warn_alert`, `token_auto_compact`,
  `token_warn` (200000), `token_critical` (400000) — gauge + alert + auto-`/compact`
  at critical when idle.
- **Approvals:** `approvals`, `auto_approve`.
- **Spawn / worktree / restart:** `spawn_gate`/`spawn_gate_max_agents`,
  `worktree_keep_done`/`worktree_auto_prune`, `pipeline_keep_done`/`pipeline_hint`,
  `auto_restart_max`/`auto_restart_reset`, `rate_limit_auto_resume`.
- **Boundary guards:** `isolation_guard`, `root_guard`, `git_redirect`,
  `check_redirect`, `git_conventions` (see git-and-checks.md).
- **Local LLM / orchestrator:** `local_llm` (+ `local_llm_url`/`_model`/`_timeout`),
  `local_llm_tier`/`_escalate`, `orchestrator`.
- **Misc:** `metrics`, `snapshots`, `insights`, `plugins`/`plugin_registry`,
  `scheduler_enabled`, `api_docs`, `collab_*`, `branch_track_*`, `tutorial`,
  `allow_nonloopback`, `log_level`/`log_format`.

> The old `WARDEN_*` env vars are no longer read (the daemon warns once if set).
> The per-agent IPC vars warden injects (`WARDEN_SESSION_ID`, `WARDEN_PIPELINE_ID`,
> `WARDEN_JOB_ID`) are not configuration.

## Remote access & authentication

Reach the dashboard/API from a phone or another device. A 256-bit token gates every
non-loopback request; binding to a non-loopback address is **refused unless a token
is set**. `warden token generate|show|rotate` (persisted `0600` to
`~/.warden/token.env`; `WARDEN_TOKEN` env overrides). Per-IP brute-force limiting; a
token-entry modal in the web UI on 401. Front the port with Tailscale / a Cloudflare
Tunnel rather than exposing it directly. Interactive OpenAPI docs at `/api/docs`.

## Web GUI & cockpit TUI

- **Web GUI** — the daemon embeds a React dashboard at `http://localhost:8765` (no
  separate server): URL-routed shell (`/cockpit` home · `/pipelines` · `/metrics` ·
  `/archive` · `/others` · `/agent/<id>`), live fleet over SSE, Cockpit Fleet
  header + agent grid, Others catch-all (attention queue with one-click approvals,
  conflicts, activity; sits last), a Metrics tab (per-agent + fleet-total CPU/memory,
  per-agent context, fleet size, tokens saved; two-column responsive layout),
  interactive terminal (xterm.js over WebSocket), agent grouping by
  Directory/Type/Status/Tag, pipeline DAG view, a header-button Context & Messages
  overlay, archive & search, theme toggle, keyboard shortcuts (`?` for help), batch
  multi-select actions.
- **Cockpit TUI** — `warden tui` (or bare `warden`): a tmux-composited cockpit with
  an agents list, a shell pane, and a live detail pane. `n` new, `s` send, `a`
  attach, `d` digest, `i` approvals, `c` context/message inspector, `x`
  terminate/cancel, `?` help; `Alt+←/→/↑/↓` move pane focus; `Alt+t` toggles the
  master pane between the orchestrator and a raw shell. Requires tmux ≥ 3.1; run
  from a plain terminal (not nested in tmux). Hold **Shift** to select text
  natively (tmux mouse mode eats plain drag).

## Interactive mode / orchestrator (`wd orch`)

warden's **interactive mode** — an operator-facing terminal REPL (aliases `wd
interactive` / `wd i`), **not** something an agent drives over MCP. A real line
editor (arrow keys, persisted history, reverse-search, a live `/`-command menu that
filters as you type, Tab completion, colour) that closes with Ctrl-D. Two ways to
drive the fleet:

- **Deterministic `/` commands (no model):** `/agents`, `/spawn <prompt>`, `/tell
  <id> <text>`, `/stop`, `/commit`/`/push`/`/sync`/`/check`, `/pipelines`, `/ctx*`,
  `/approvals`, … `/help` lists them. Reads auto-execute; mutations pass the confirm
  gate. Works even when the local model misbehaves.
- **Guided argument forms:** when a `/` command needs more than was typed, warden
  collects the args interactively — numbered pick-lists for known-set fields (model,
  permission_mode, type, yes/no), free text otherwise. Auto-opens for a missing
  required arg (bare `/spawn`); a `+` suffix (`/spawn+ <prompt>`) opens the full
  form. Deterministic structure; a local model, if present, pre-fills each field
  with a suggestion (Enter accepts, type overrides, `-` clears).
- **Natural language (local LLM):** any other line is planned into **confirmed**
  warden tool calls — no Claude tokens. **It conducts, never implements** (no
  edit/write/bash in its registry — code work is delegated by spawning a Claude
  agent).

Starts without a model (the `/` commands and `!`-shell always work); only the NL
half needs `local_llm: true`. `!`-prefixed lines run in a persistent embedded shell,
reported verbatim (no auto-action). Run standalone or as the cockpit master pane
(`orchestrator` config / `--orch`).

**Picking the local model — `wd llm suggest`.** Auto-detects the machine's **total**
and **average free** memory (same pool: NVIDIA VRAM / Apple unified / Linux
`MemAvailable`, free sampled a few times) and prints a memory-ranked shortlist,
marking each `fits now` / `free memory first` / `too large`. It scores a
tool-calling-forward catalog (Qwen3, gpt-oss, Mistral Small, Qwen2.5) by
**conductor suitability** — calibrated against the Berkeley Function-Calling
Leaderboard (BFCL v4, multi-turn-weighted), since the orchestrator routes tool
calls and never writes code, so size/coding skill is the wrong axis — and stars
the best model that runs comfortably now with headroom. Flags: `--samples`, `--total-gb`/`--free-gb`
overrides, `--json`. `wd doctor` prints the one-line version. Recommendation only —
set `local_llm_model` by hand (no `config set`; `wd config path` locates the YAML).

## Export / import & plugins

- **Export/import:** MCP `export_sessions {all?}` / `import_sessions {data, merge?}`
  (CLI `warden export`/`import`). Export dumps active records as a versioned JSON
  envelope (`all` includes the archive); import is **idempotent by id** (`merge`
  overwrites collisions). Worktrees/branches/tmux are **not** serialized or
  recreated — an imported record just remembers where its worktree used to live.
- **Plugins:** extend warden with **custom agent task types** and **lifecycle
  hooks** via an external executable over JSON-over-stdio. Default **off** (`plugins`
  gate; plugins run external code). Hooks are advisory and fail-open (a `pre-` hook
  cannot veto). MCP `list_plugins` (CLI `wd plugin list`) shows registered plugins,
  paths, custom types, and subscribed events. Configure with `plugins` +
  `plugin_registry`; example under `examples/plugins/`.

## Fleet inspection — search, history, metrics, worktrees

Read verbs for catching up on the fleet, all MCP-first:

- `search {query, closed?}` (CLI `wd search`) — full-text across agents (subject,
  prompt, type, name, pane, id, ticket, branch); `closed` also searches the archive.
- `history {since?, type?, limit?}` (CLI `wd history`) — browse archived agents.
- `get_metrics {history?, since?, limit?}` (CLI `wd stats`) — live resource snapshot
  or time-series; `get_pressure` is the spawn gate's memory-headroom verdict.
- `list_worktrees {repo?}` / `prune_worktrees {repo?, dry_run?, force?}` (CLI
  `wd worktree ls` / `wd prune`) — list / reconcile a repo's worktrees.
- `digest {ticket}` (CLI `wd digest`) — a compact catch-up summary of one agent.
