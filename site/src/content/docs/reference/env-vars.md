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
| `model_default` | `claude-sonnet-4-6` | Default model for new agents (passed through verbatim to the AI CLI) |
| `default_permission_mode` | `auto` | Default permission mode for new agents (`auto`/`default`/`acceptEdits`/`bypassPermissions`/`dontAsk`/`plan`) |
| `git.protected_branches` | `main`, `master` | Branches lifecycle commands refuse to commit, push, or use as a PR head. Setting this list replaces those two defaults. |
| `git.protect_default_branch` | `true` | Also protect the repository's remote default branch (for example `develop`) even when `git.protected_branches` is customized. Set `false` only when that branch may be changed by lifecycle commands. Both Git settings are hot-reloaded. |
| `notify.enabled` | `false` | macOS/libnotify desktop notifications when an agent needs attention |
| `notify.webhook_enabled` / `notify.webhook_url` | `false` / _(empty)_ | POST notifications to a webhook (a Slack incoming-webhook URL works out of the box) |
| `approvals` | `true` | The approvals inbox: parse recognized tool-permission prompts and surface them for one-click answers |
| `auto_approve` | `false` | Auto-answer recognized prompts. Bare on/off, or an allow/deny rule policy (by tool / glob / regex / paths, with per-agent overrides); manage with `warden approval auto set` |
| `trust_workspace` | `true` | Automatically answer an AI CLI's launch-time "do you trust this folder?" prompt for every agent warden launches (Claude, Codex, Antigravity; Cursor is launched with `--trust`). Independent of `auto_approve`. `false` leaves the prompt for the approvals inbox |
| `recognize_prompts` | `true` | When an agent sits on a choice menu no backend parser recognizes (an AI CLI reworded its prompt), have the Fast-Brain model read the pane and identify it, so it reaches the approvals inbox and the auto-approve policy. The model only recognizes; its reading is verified against the pane and then follows the unchanged destructive guard, rules and circuit breaker. One model call per stalled, unrecognized menu. |
| `known_prompts_max` | `500` | Most prompt shapes the learned-prompt store keeps. A model-read prompt is learned on its first verified success (answered, then the menu is gone) so the next occurrence needs no model call; the store self-heals (a shape that trips the circuit breaker or repeatedly fails to clear the menu is dropped, with a `prompt_known_invalidated` event). Past the bound the least recently seen shape is evicted (`0` = unbounded) |
| `known_prompts_prune_days` | `90` | Learned prompt shapes not seen for this many days are dropped (`0` = never) |
| `auto_approve.max_repeats` | `10` | Circuit breaker: consecutive identical approvals allowed per agent before auto-approve halts and escalates to a human (`0` = default, negative = off) |
| `http.timeout_fast` / `http.timeout_slow` | `30s` / `10m` | Daemon write budgets: fast bounds ordinary data/action routes; slow bounds lifecycle routes (spawn's worktree checkout, commit/push hooks, checks). Backstops against a wedged handler — keep generous, especially in large monorepos |
| `tokens.guard` | `true` | Context-size guard master switch (gauge + alert + auto-compact) |
| `tokens.warn_alert` | `true` | Fire a desktop notification once per upward crossing into warning/critical |
| `tokens.auto_compact` | `true` | Auto-send `/compact` when an agent is `critical` and idle/waiting |
| `tokens.force_compact` | `false` | Interrupt a `critical` **busy** agent, `/compact`, then resume it (destructive). Per-agent override via `warden agent set <id> compact` |
| `tokens.compact_resume_prompt` | _(built-in)_ | Resume message sent to a force-compacted agent once compaction lands |
| `tokens.warn` | `200000` | Warning threshold in context tokens (inclusive) |
| `tokens.critical` | `400000` | Critical threshold in context tokens (inclusive) — the auto-`/compact` band |
| `local_llm.*` | — | **Retired.** The local-LLM/Ollama provider was replaced by Fast-Brain (no config). Legacy keys (`enabled`, `url`, `model`, `timeout`, `tier`, `escalate`, `classifier`, `repl`) still parse but are ignored. |
| `backends.limit_retry` | `15m` | Go duration — how long a free CLI backend is skipped after a rate-limit / spend signal, before retrying it. Backend **tiers**, the **default**, and **enabled** flags live in the [backend registry](/warden/guides/backend-registry/) store (`~/.warden/backends`), not this file — edit them with `warden backend …`, the web 🧩 panel, or the TUI |
| `metrics` | `true` | Record per-agent performance history for `warden inspect resources --history` |
| `spawn_gate` / `spawn_gate_max_agents` | `true` / `0` | Memory-pressure spawn gate + concurrent-agent cap (0 = no cap). Blocks a spawn only at **critical** pressure or the agent cap; **warn** pressure is advisory (spawns proceed). |
| `pipeline.keep_done` / `pipeline.hint` | — | Pipeline retention + the decomposition nudge |
| `memory.inject` | `true` | Project the repo's curated `.warden/memory.md` into every spawned agent's system prompt (Claude → `--append-system-prompt`; other backends → their `AGENTS.md`/`CRUSH.md`/`.goosehints` warden block). Off, or an empty/absent file, is byte-identical to no injection. See [Project memory](/warden/concepts/project-memory/) |
| `router.use_fast_brain` | `false` | Opt-in prompt-complexity tier routing: for a spawn that pins no tier, task, role, model or ai_cli, Fast-Brain rates the prompt (`tier-1` trivial tweaks / `tier-2` standard / `tier-3` deep refactors) and the router uses it only at confidence ≥ 0.8. Lowest-precedence input; recorded as a `tier-route` event on the agent. Hot-reloaded |
| `memory.curate` | `false` | Auto-propose durable memory entries from completion digests into `.warden/memory.md`. A debounced pass writes **`unverified`, timestamped, provenance-tagged** proposals to the **working tree only** — it never commits or pushes, so the committed diff is the human review gate. Proposals promote to `trusted` only on corroboration; contradictions supersede (tombstone) older entries; un-recorroborated entries age out; vanished paths are flagged stale. Runs on Fast-Brain (latency-bounded, fail-open). Opt-in. See [Project memory](/warden/concepts/project-memory/) |
| `savings` | `true` | Record the token-savings ledger (`warden usage savings`, `GET /api/v1/savings`) |
| `savings_samples` | `false` | Retain raw-vs-kept provenance samples for `warden usage savings --audit` (may hold sensitive output) |
| `scheduler_enabled` | `false` | Enable the native cron/at scheduler (`warden schedule create/list/show/run/edit/enable/disable/delete`) |
| `collab.enabled` | `true` | File-conflict detection across agent worktrees |
| `collab.interval` | `10s` | Watch-reconcile + in-memory conflict scan interval |
| `collab.git_reconcile_interval` | `2m` | Git-diff backstop when fsnotify is active (polling-only mode uses `collab.interval` instead) |
| `collab.hint` | `true` | Append the conflict-check coordination hint to spawned agents |
| `branch_track.enabled` | `false` | Enable the per-agent branch monitor (`warden workspace branches`) |
| `branch_track.interval` | `2m` | Poll interval for the branch monitor |
| `activity.interval` | `15s` | Minimum gap between live activity-badge refreshes per agent (the 3-5 word status badge on each TUI agent row). Refreshes only while the agent's pane is changing, so idle agents cost no Fast-Brain calls; a failed/empty decision keeps the previous badge |
| `snapshots` | `true` | Enable the worktree+transcript checkpoint store (`warden workspace snapshot`) |
| `insights` | `true` | Enable history-mined insights (`warden usage insights`) |
| `tutorial` | `true` | Show the first-run walkthrough nudge (`warden tutorial`) |
| `api_docs` | `true` | Serve the OpenAPI spec + Swagger UI at `/api/docs` |
| `plugins.enabled` | `false` | Enable the plugin system. **Default off** — plugins run external code |
| `plugins.registry` | _(empty)_ | List of registered plugins (name, path, events, task_types). Only used when `plugins.enabled` is on |
| `allow_nonloopback` | `false` | **Deprecated / inert** — no longer bypasses auth. A token is mandatory for any non-loopback bind; setting this only logs a deprecation warning |
| `log.level` / `log.format` | `info` / `text` | Daemon log verbosity and format (`text`/`json`) |
| `rate_limit.recovery.enabled` | `true` | Reactive hard-limit recovery master switch (falls back to same-backend auto-resume when false) |
| `rate_limit.recovery.stabilization_window` | `10s` | How long a replacement candidate must stay live and non-limited before recovery clears |
| `rate_limit.recovery.usage_reconciliation.enabled` | `false` | Opt-in provider usage polling + bulk bucket reconciliation. When false, the daemon makes no provider usage network calls; `wd usage recover` still works |
| `rate_limit.recovery.usage_reconciliation.interval` | `60s` | Poll cadence when usage reconciliation is enabled |
| `rate_limit.recovery.usage_reconciliation.stale_after` | `15m` | Freshness window — only successful snapshots inside this window may force exhaustion |
| `rate_limit.recovery.usage_reconciliation.max_parallel_swaps` | `3` | Bounded concurrency for bulk recovery candidate selection/launch |

`autopilot.completion.merge_poll_interval` (default `2m`, hot-reloaded, floor `30s`) sets how often a green autopilot final PR is polled while the run is awaiting your merge; `autopilot.completion.merge_default` / `manager_verify_timeout` are its siblings (see the [autopilot guide](/warden/guides/autopilot/)).

There are more (`auto_restart.*`, `rate_limit.*`, `worktree.keep_done` /
`worktree.auto_prune`, …) — `warden config` is the authoritative, live list.

Related settings are grouped into namespaced blocks (`pipeline.*`, `auto_restart.*`,
`collab.*`, `memory.*`, `branch_track.*`, `activity.*`, `rate_limit.*`, `http.*`, `log.*`,
`plugins.*`, `backends.*`, alongside `rails.*` / `tokens.*` / `notify.*` /
`worktree.*`; the retired `local_llm.*` is parsed but ignored). The `autopilot.brain.backends` ladder and
`autopilot.brain.allow_pay_per_use` gate are **deprecated** — the [backend
registry](/warden/guides/backend-registry/) store is now their source of truth (the
keys are imported once on the first boot after upgrade, then ignored). The old flat
keys (`collab_enabled`, `log_level`, `memory_inject`, …)
still load as **deprecated aliases** — they work but emit a one-time deprecation
warning, and `warden config` rewrites them into the nested form on its next run.
