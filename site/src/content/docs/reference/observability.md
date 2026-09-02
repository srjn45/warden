---
title: Observability & metrics
description: Resource metrics, the warden inspect resources CLI, notifications, and the context-size guard.
---

All toggles below are **config-file settings** (`~/.warden/config.yaml`) — run
`warden config` to see the live values. (The old `WARDEN_*` environment variables
for these are no longer read; see [Configuration & environment](/warden/reference/env-vars/).)

| Feature | Description |
|---|---|
| **Resource metrics** | `internal/metrics` collects per-agent process-tree RSS/CPU, system memory/swap/pressure, and daemon self-stats. Exposed via `GET /api/v1/metrics` + `GET /api/v1/metrics/history`. |
| **`warden inspect resources`** | CLI view of the resource metrics (`--watch` to refresh, `--history` for persisted per-agent performance history + anomaly warnings). |
| **Metrics recorder** | Optional JSONL recorder of per-agent performance history (`metrics` setting, default on) — powers `warden inspect resources --history`. |
| **Desktop notifications** | `notify.enabled: true` posts a desktop notification (macOS/libnotify) when an agent needs attention (`waiting_for_input`, stuck `idle`, `orphaned`, `errored`). |
| **Webhook / Slack** | `notify.webhook_enabled: true` + `notify.webhook_url` POSTs the same alerts to a webhook (a Slack incoming-webhook URL works out of the box). The URL must be `http`/`https`; warden never follows redirects and refuses link-local targets (e.g. the `169.254.169.254` cloud-metadata endpoint), while loopback/LAN relays stay allowed. |
| **Context-size guard** | `internal/ctxtokens` reads each live agent's context-window fill from its transcript and classifies it `ok`/`warning`/`critical`. The poller shows a state-colored token figure in `ls`/TUI/web, alerts once per upward crossing (`tokens.warn_alert`), and auto-sends `/compact` at `critical` when the agent is idle (`tokens.auto_compact`, cooldown-guarded). Master switch `tokens.guard`; thresholds `tokens.warn` (200k) / `tokens.critical` (400k). |
| **Force-compact (busy agents)** | Opt-in (`tokens.force_compact`, off by default). For the case `tokens.auto_compact` can't touch — an agent that goes `critical` while **still working** — the poller mirrors the manual fix as a state machine: interrupt the agent (Escape), `/compact` once it idles, then send `tokens.compact_resume_prompt` to resume the discarded work. **Destructive** (the in-flight turn is lost). Per-agent override: `warden agent compact set <id> on\|off\|inherit` (or the `set_force_compact` MCP tool). An interrupt that doesn't take within ~45s is abandoned, falling back to the pre-crash nudge. |
| **Branch tracking** (`warden workspace branches`) | Opt-in monitor (`branch_track.enabled`, interval `branch_track.interval`) reporting each active agent's **GitHub CI status** and **standing vs `origin/main`**. Alerts are non-blocking: an inbox note (+ desktop ping) on a new CI failure, an inbox nudge on a merged or far-behind branch. Every `gh`/git call fails open. Read-only via `warden workspace branches`, `GET /api/v1/collab/branches`, or the `get_branch_status` MCP tool. |
| **Token-savings ledger** (`warden usage savings`) | A real, append-only ledger of the tokens warden's lifecycle features kept out of agents' context. Gated by `savings` (default on). See [Token-savings ledger](/warden/reference/savings/). |
| **Insights** (`warden usage insights`) | History-mined patterns and parallelization opportunities. Gated by `insights` (default on). See [Insights](/warden/reference/insights/). |

The web dashboard surfaces these in the **Metrics** tab (`/metrics`): per-agent **CPU** and **Memory** charts each paired with a fleet-wide **Total CPU** / **Total memory** line, a per-agent **Context** chart, a **fleet-size** trend, a **Tokens saved** trend (from the savings ledger, with a window picker that buckets by hour for short windows and day for longer ones, plus a running-cumulative line), a **Savings by feature** stacked breakdown, and a **Live footprint** card with the live per-agent + system resource charts (uPlot). The cards are a responsive grid — two columns on wide screens, one on mobile. See [Web mission control → Metrics view](/warden/guides/web-mission-control/#metrics-view).

```sh
warden inspect resources           # one-shot view of the fleet's resource footprint
warden inspect resources --watch   # redraw every 3s until interrupted
warden inspect resources --history # persisted per-agent performance history + anomaly warnings
warden inspect resources --json    # machine-readable
warden workspace branches        # per-agent CI + base-branch standing (opt-in monitor)
```
