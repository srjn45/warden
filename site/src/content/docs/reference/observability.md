---
title: Observability & metrics
description: Resource metrics, the warden stats CLI, notifications, and the context-size guard.
---

All toggles below are **config-file settings** (`~/.warden/config.yaml`) — run
`warden config` to see the live values. (The old `WARDEN_*` environment variables
for these are no longer read; see [Configuration & environment](/warden/reference/env-vars/).)

| Feature | Description |
|---|---|
| **Resource metrics** | `internal/metrics` collects per-agent process-tree RSS/CPU, system memory/swap/pressure, and daemon self-stats. Exposed via `/metrics` + `/metrics/history`. |
| **`warden stats`** | CLI view of the resource metrics (`--watch` to refresh, `--history` for persisted per-agent performance history + anomaly warnings). |
| **Metrics recorder** | Optional JSONL recorder of per-agent performance history (`metrics` setting, default on) — powers `warden stats --history`. |
| **Desktop notifications** | `notify: true` posts a desktop notification (macOS/libnotify) when an agent needs attention (`waiting_for_input`, stuck `idle`, `orphaned`, `errored`). |
| **Webhook / Slack** | `webhook_enabled: true` + `webhook_url` POSTs the same alerts to a webhook (a Slack incoming-webhook URL works out of the box). |
| **Context-size guard** | `internal/ctxtokens` reads each live agent's context-window fill from its transcript and classifies it `ok`/`warning`/`critical`. The poller shows a state-colored token figure in `ls`/TUI/web, alerts once per upward crossing (`token_warn_alert`), and auto-sends `/compact` at `critical` when the agent is idle (`token_auto_compact`, cooldown-guarded). Master switch `token_guard`; thresholds `token_warn` (200k) / `token_critical` (400k). |
| **Branch tracking** (`warden branches`) | Opt-in monitor (`branch_track_enabled`, interval `branch_track_interval`) reporting each active agent's **GitHub CI status** and **standing vs `origin/main`**. Alerts are non-blocking: an inbox note (+ desktop ping) on a new CI failure, an inbox nudge on a merged or far-behind branch. Every `gh`/git call fails open. Read-only via `warden branches`, `GET /collab/branches`, or the `get_branch_status` MCP tool. |
| **Token-savings ledger** (`warden savings`) | A real, append-only ledger of the tokens warden's lifecycle features kept out of agents' context. Gated by `savings` (default on). See [Token-savings ledger](/warden/reference/savings/). |
| **Insights** (`warden insights`) | History-mined patterns and parallelization opportunities. Gated by `insights` (default on). See [Insights](/warden/reference/insights/). |

The web dashboard surfaces the resource metrics as a **Resources panel** with live per-agent + system resource charts (uPlot).

```sh
warden stats           # one-shot view of the fleet's resource footprint
warden stats --watch   # redraw every 3s until interrupted
warden stats --history # persisted per-agent performance history + anomaly warnings
warden stats --json    # machine-readable
warden branches        # per-agent CI + base-branch standing (opt-in monitor)
```
