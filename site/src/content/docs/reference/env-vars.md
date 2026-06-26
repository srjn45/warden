---
title: Configuration & environment
description: Every warden config-file setting, its default, and what it controls.
---

Warden reads all settings from a single **config file** (`~/.warden/config.yaml`).
Run `warden config init` to generate a fully-commented file, edit the values, then
restart the daemon; `warden config` prints what's live and the file path. The
`--config <path>` flag points any command at an alternate file, and `--addr
<host:port>` overrides the daemon address for a single command.

:::caution[Legacy `WARDEN_*` env vars are no longer read]
Warden used to read scattered `WARDEN_*` / `AGENTCTL_*` environment variables.
Those are now **ignored** — the daemon logs a one-time warning at startup if any are
still set. Move them into the config file. The one deliberate exception is
**`WARDEN_TOKEN`** (the remote-access bearer token), which stays an env var so the
secret never lands in the config file. The per-agent IPC vars warden injects into
each agent (`WARDEN_SESSION_ID`, `WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`) are runtime
plumbing, not configuration.
:::

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `WARDEN_TOKEN` | unset | Bearer token for remote (non-loopback) access — clients send it, the daemon requires it when bound off-loopback. Manage with `warden token`. See [Remote access](/warden/guides/remote-access/) |
| `ANTHROPIC_API_KEY` | unset | Used only by `warden savings --calibrate` to call Claude's `count_tokens` endpoint. Never printed; calibration is the only command that reads it |

## Config-file settings

Common settings (run `warden config` for the complete, live list):

| Setting | Default | Description |
|---|---|---|
| `addr` | `127.0.0.1:8765` | Daemon listen/connect address. A non-loopback address requires `WARDEN_TOKEN`, or `allow_nonloopback: true` to bind without auth |
| `data_dir` | `~/.warden` | Directory for warden state: session JSON (`sessions/`, `closed/`), prompt files, inbox, pipelines, snapshots, savings ledger, and metrics |
| `claude_projects_dir` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects and the context gauge |
| `model_default` | `claude-sonnet-4-6` | Default model for new agents (a model id or alias: `sonnet`/`opus`/`haiku`/`fable`) |
| `default_permission_mode` | `auto` | Default permission mode for new agents (`auto`/`default`/`acceptEdits`/`bypassPermissions`/`dontAsk`/`plan`) |
| `notify` | `false` | macOS/libnotify desktop notifications when an agent needs attention |
| `webhook_enabled` / `webhook_url` | `false` / _(empty)_ | POST notifications to a webhook (a Slack incoming-webhook URL works out of the box) |
| `approvals` | `true` | The approvals inbox: parse recognized tool-permission prompts and surface them for one-click answers |
| `auto_approve` | `false` | Auto-answer recognized yes/no permission prompts (global default; per-agent override via `warden auto-approve`) |
| `token_guard` | `true` | Context-size guard master switch (gauge + alert + auto-compact) |
| `token_warn_alert` | `true` | Fire a desktop notification once per upward crossing into warning/critical |
| `token_auto_compact` | `true` | Auto-send `/compact` when an agent is `critical` and idle/waiting |
| `token_warn` | `200000` | Warning threshold in context tokens (inclusive) |
| `token_critical` | `400000` | Critical threshold in context tokens (inclusive) — the auto-`/compact` band |
| `local_llm` | `false` | Enable the local-LLM provider (orchestrator REPL, commit-message/insights narration, classify/summarize offload) |
| `metrics` | `true` | Record per-agent performance history for `warden stats --history` |
| `spawn_gate` / `spawn_gate_max_agents` | `true` / `0` | Memory-pressure spawn gate and a hard cap on concurrent agents (0 = no cap) |
| `pipeline_keep_done` / `pipeline_hint` | — | Pipeline retention + the decomposition nudge |
| `savings` | `true` | Record the token-savings ledger (`warden savings`, `GET /api/v1/savings`) |
| `savings_samples` | `false` | Retain raw-vs-kept provenance samples for `warden savings --audit` (may hold sensitive output) |
| `scheduler_enabled` | `false` | Enable the native cron/at scheduler (`warden schedule`) |
| `branch_track_enabled` | `false` | Enable the per-agent branch monitor (`warden branches`) |
| `branch_track_interval` | `2m` | Poll interval for the branch monitor |
| `snapshots` | `true` | Enable the worktree+transcript checkpoint store (`warden snapshot`) |
| `insights` | `true` | Enable history-mined insights (`warden insights`) |
| `tutorial` | `true` | Show the first-run walkthrough nudge (`warden tutorial`) |
| `api_docs` | `true` | Serve the OpenAPI spec + Swagger UI at `/api/docs` |
| `plugins` | `false` | Enable the plugin system. **Default off** — plugins run external code |
| `plugin_registry` | _(empty)_ | List of registered plugins (name, path, events, task_types). Only used when `plugins` is on |
| `allow_nonloopback` | `false` | Bind a non-loopback address *without* a token (not recommended) |
| `log_level` / `log_format` | `info` / `text` | Daemon log verbosity and format (`text`/`json`) |

There are more (`auto_restart_*`, `rate_limit_*`, `worktree_keep_done` /
`worktree_auto_prune`, …) — `warden config` is the authoritative, live list.
