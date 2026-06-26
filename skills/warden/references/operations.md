# warden — operating the fleet (insights, audit, scheduler, config, remote, UIs)

The cross-cutting capabilities that run the fleet rather than a single agent.

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

## Audit log (CLI-only)

`warden audit log` — an append-only trail of meaningful daemon actions (`spawn`,
`terminate`, `delete`, `approve`, `pipeline_start`/`pipeline_cancel`,
`schedule_create`/`schedule_delete`) at `~/.warden/audit.jsonl` (`0600`). Each
record: `time`, `action`, `actor`, `target`, action-specific `detail`. Flags:
`--tail N` (default 50, `0`=all), `--action`, `--target`, `--since`/`--until`,
`--json`. Reads the file directly, so it works even while the daemon is down.

## Scheduler — recurring / single-shot triggers

Daemon-fired triggers — no external crontab. **Opt-in:** gated by
`scheduler_enabled` (default **off**); routes return 403 and the loop is a no-op
until enabled (schedules only fire while the daemon runs). MCP `list_schedules` is
**read-only** (403 when disabled); create/delete are CLI-only.

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
prints what's live. `wd config set <key> <value>` sets one key. `--config <path>`
selects an alternate file; `--addr <host:port>` overrides the daemon address
per-command.

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
  separate server): live fleet over SSE, attention queue with one-click approvals,
  interactive terminal (xterm.js over WebSocket), agent grouping by
  Directory/Type/Status/Tag, pipeline DAG view, digest/resources panels, archive &
  search, theme toggle, keyboard shortcuts (`?` for help), batch multi-select
  actions.
- **Cockpit TUI** — `warden tui` (or bare `warden`): a tmux-composited cockpit with
  an agents list, a shell pane, and a live detail pane. `n` new, `s` send, `a`
  attach, `d` digest, `i` approvals, `c` context/message inspector, `x`
  terminate/cancel, `?` help; `Alt+←/→/↑/↓` move pane focus; `Alt+t` toggles the
  master pane between the orchestrator and a raw shell. Requires tmux ≥ 3.1; run
  from a plain terminal (not nested in tmux). Hold **Shift** to select text
  natively (tmux mouse mode eats plain drag).

## Local-LLM orchestrator (`wd orch`)

A warden-aware, **local-LLM** conductor REPL that turns natural-language operator
intent into **confirmed** warden tool calls — no Claude tokens. Requires
`local_llm: true`. **It conducts, never implements** (no edit/write/bash in its
registry — all code work is delegated by spawning a Claude agent). Read-only verbs
auto-execute; every mutating verb requires explicit operator confirmation (a
non-config-gated gate). `!`-prefixed lines run in a persistent embedded shell and
are reported verbatim (no auto-action). Run standalone (`wd orch`) or as the cockpit
master pane (`orchestrator` config / `--orch`).

## Export / import & plugins

- **Export/import (CLI):** `warden export` dumps active records as a versioned JSON
  envelope (`--all` includes the archive); `warden import` reads it from stdin,
  **idempotent by id** (`--merge` overwrites collisions). Worktrees/branches/tmux
  are **not** serialized or recreated — an imported record just remembers where its
  worktree used to live.
- **Plugins (`wd plugin`):** extend warden with **custom agent task types** and
  **lifecycle hooks** via an external executable over JSON-over-stdio. Default
  **off** (`plugins` gate; plugins run external code). Hooks are advisory and
  fail-open (a `pre-` hook cannot veto). `wd plugin list` shows registered plugins,
  paths, custom types, and subscribed events. Configure with `plugins` +
  `plugin_registry`; example under `examples/plugins/`.
