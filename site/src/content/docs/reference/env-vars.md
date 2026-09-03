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
still set. Move them into the config file. The deliberate exceptions are
**`WARDEN_TOKEN`** and **`WARDEN_READONLY_TOKEN`** (the remote-access bearer
tokens), which stay env vars so the secrets never land in the config file. The per-agent IPC vars warden injects into
each agent (`WARDEN_SESSION_ID`, `WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`) are runtime
plumbing, not configuration.
:::

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `WARDEN_TOKEN` | unset | Bearer token for remote (non-loopback) access — clients send it, the daemon requires it when bound off-loopback. Manage with `warden daemon token`. See [Remote access](/warden/guides/remote-access/) |
| `WARDEN_READONLY_TOKEN` | unset | Optional read-only bearer token: may read everything (GETs + the live event stream) but every write and the interactive attach return `403`. Only honored alongside `WARDEN_TOKEN`. Print it with `warden daemon token show --readonly`. See [Remote access](/warden/guides/remote-access/) |
| `ANTHROPIC_API_KEY` | unset | Used only by `warden usage savings --calibrate` to call Claude's `count_tokens` endpoint. Never printed; calibration is the only command that reads it |

## Config-file settings

Common settings (run `warden config` for the complete, live list):

| Setting | Default | Description |
|---|---|---|
| `addr` | `127.0.0.1:8765` | Daemon listen/connect address. A non-loopback address **requires** `WARDEN_TOKEN`; the daemon refuses a non-loopback bind without a token |
| `trusted_proxies` | _(none)_ | Reverse proxies / tunnels fronting the daemon (IPs/CIDRs). When the peer is one of these, the audit log resolves the real client from `X-Forwarded-For`. Audit-actor only — the auth throttle still keys on the peer IP. See [Remote access](/warden/guides/remote-access/) |
| `data_dir` | `~/.warden` | Directory for warden state: the embedded ScrivaDB session store (`sessions-db/`, with a one-time-imported read-only JSON backup in `sessions/`+`closed/`), prompt files, inbox, pipelines, snapshots, savings ledger, and metrics |
| `claude_projects_dir` | `~/.claude/projects` | Where the poller reads transcripts to generate subjects and the context gauge |
| `model_default` | `claude-sonnet-4-6` | Default model for new agents (a model id or alias: `sonnet`/`opus`/`haiku`/`fable`) |
| `default_permission_mode` | `auto` | Default permission mode for new agents (`auto`/`default`/`acceptEdits`/`bypassPermissions`/`dontAsk`/`plan`) |
| `notify.enabled` | `false` | macOS/libnotify desktop notifications when an agent needs attention |
| `notify.webhook_enabled` / `notify.webhook_url` | `false` / _(empty)_ | POST notifications to a webhook (a Slack incoming-webhook URL works out of the box) |
| `approvals` | `true` | The approvals inbox: parse recognized tool-permission prompts and surface them for one-click answers |
| `auto_approve` | `false` | Auto-answer recognized prompts. Bare on/off, or an allow/deny rule policy (by tool / glob / regex / paths, with per-agent overrides); manage with `warden approval auto set` |
| `auto_approve.max_repeats` | `10` | Circuit breaker: consecutive identical approvals allowed per agent before auto-approve halts and escalates to a human (`0` = default, negative = off) |
| `http.timeout_fast` / `http.timeout_slow` | `30s` / `10m` | Daemon write budgets: fast bounds ordinary data/action routes; slow bounds lifecycle routes (spawn's worktree checkout, commit/push hooks, checks). Backstops against a wedged handler — keep generous, especially in large monorepos |
| `tokens.guard` | `true` | Context-size guard master switch (gauge + alert + auto-compact) |
| `tokens.warn_alert` | `true` | Fire a desktop notification once per upward crossing into warning/critical |
| `tokens.auto_compact` | `true` | Auto-send `/compact` when an agent is `critical` and idle/waiting |
| `tokens.force_compact` | `false` | Interrupt a `critical` **busy** agent, `/compact`, then resume it (destructive). Per-agent override via `warden agent compact set` |
| `tokens.compact_resume_prompt` | _(built-in)_ | Resume message sent to a force-compacted agent once compaction lands |
| `tokens.warn` | `200000` | Warning threshold in context tokens (inclusive) |
| `tokens.critical` | `400000` | Critical threshold in context tokens (inclusive) — the auto-`/compact` band |
| `local_llm.enabled` | `false` | Enable the local-LLM provider (REPL, commit-message/insights narration, classify/summarize offload) |
| `backends.limit_retry` | `15m` | Go duration — how long the internal free/local **thinking router** skips a free CLI backend after a rate-limit / spend signal, before retrying it. Backend **tiers**, the **default**, **enabled** flags, and the **thinking mode** live in the [backend registry](/warden/guides/backend-registry/) store (`~/.warden/backends`), not this file — edit them with `warden backend …`, the web 🧩 panel, or the TUI |
| `metrics` | `true` | Record per-agent performance history for `warden inspect resources --history` |
| `spawn_gate` / `spawn_gate_max_agents` | `true` / `0` | Memory-pressure spawn gate + concurrent-agent cap (0 = no cap). Blocks a spawn only at **critical** pressure or the agent cap; **warn** pressure is advisory (spawns proceed). |
| `pipeline.keep_done` / `pipeline.hint` | — | Pipeline retention + the decomposition nudge |
| `memory.inject` | `true` | Project the repo's curated `.warden/memory.md` into every spawned agent's system prompt (Claude → `--append-system-prompt`; other backends → their `AGENTS.md`/`CRUSH.md`/`.goosehints` warden block). Off, or an empty/absent file, is byte-identical to no injection. See [Project memory](/warden/concepts/project-memory/) |
| `memory.curate` | `false` | Auto-propose durable memory entries from completion digests into `.warden/memory.md`. A debounced pass writes **`unverified`, timestamped, provenance-tagged** proposals to the **working tree only** — it never commits or pushes, so the committed diff is the human review gate. Proposals promote to `trusted` only on corroboration; contradictions supersede (tombstone) older entries; un-recorroborated entries age out; vanished paths are flagged stale. Prefers the `$0` local model, degrading to `claude -p` only where configured. Opt-in. See [Project memory](/warden/concepts/project-memory/) |
| `savings` | `true` | Record the token-savings ledger (`warden usage savings`, `GET /api/v1/savings`) |
| `savings_samples` | `false` | Retain raw-vs-kept provenance samples for `warden usage savings --audit` (may hold sensitive output) |
| `scheduler_enabled` | `false` | Enable the native cron/at scheduler (`warden schedule`) |
| `collab.enabled` | `true` | File-conflict detection across agent worktrees |
| `collab.interval` | `10s` | Watch-reconcile + in-memory conflict scan interval |
| `collab.git_reconcile_interval` | `2m` | Git-diff backstop when fsnotify is active (polling-only mode uses `collab.interval` instead) |
| `collab.hint` | `true` | Append the conflict-check coordination hint to spawned agents |
| `branch_track.enabled` | `false` | Enable the per-agent branch monitor (`warden workspace branches`) |
| `branch_track.interval` | `2m` | Poll interval for the branch monitor |
| `snapshots` | `true` | Enable the worktree+transcript checkpoint store (`warden workspace snapshot`) |
| `insights` | `true` | Enable history-mined insights (`warden usage insights`) |
| `tutorial` | `true` | Show the first-run walkthrough nudge (`warden tutorial`) |
| `api_docs` | `true` | Serve the OpenAPI spec + Swagger UI at `/api/docs` |
| `plugins.enabled` | `false` | Enable the plugin system. **Default off** — plugins run external code |
| `plugins.registry` | _(empty)_ | List of registered plugins (name, path, events, task_types). Only used when `plugins.enabled` is on |
| `allow_nonloopback` | `false` | **Deprecated / inert** — no longer bypasses auth. A token is mandatory for any non-loopback bind; setting this only logs a deprecation warning |
| `log.level` / `log.format` | `info` / `text` | Daemon log verbosity and format (`text`/`json`) |

There are more (`auto_restart.*`, `rate_limit.*`, `worktree.keep_done` /
`worktree.auto_prune`, …) — `warden config` is the authoritative, live list.

Related settings are grouped into namespaced blocks (`pipeline.*`, `auto_restart.*`,
`collab.*`, `memory.*`, `branch_track.*`, `rate_limit.*`, `http.*`, `log.*`,
`plugins.*`, `backends.*`, alongside `rails.*` / `tokens.*` / `notify.*` /
`worktree.*` / `local_llm.*`). The `autopilot.brain.backends` ladder and
`autopilot.brain.allow_pay_per_use` gate are **deprecated** — the [backend
registry](/warden/guides/backend-registry/) store is now their source of truth (the
keys are imported once on the first boot after upgrade, then ignored). The old flat
keys (`collab_enabled`, `log_level`, `memory_inject`, …)
still load as **deprecated aliases** — they work but emit a one-time deprecation
warning, and `warden config` rewrites them into the nested form on its next run.
