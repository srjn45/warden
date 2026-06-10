---
title: Observability & metrics
description: Resource metrics, the warden stats CLI, notifications, and the context-size guard.
---

| Feature | Description |
|---|---|
| **Resource metrics** | `internal/metrics` collects per-agent process-tree RSS/CPU, system memory/swap/pressure, and daemon self-stats. Exposed via `/metrics` + `/metrics/history`. |
| **`warden stats`** | CLI view of the resource metrics. |
| **Metrics recorder** | Optional 15s JSONL recorder (`WARDEN_METRICS`). |
| **macOS notifications** | `WARDEN_NOTIFY=on` posts a desktop notification when an agent needs attention (`waiting_for_input`, stuck `idle`, `orphaned`, `errored`). |
| **Context-size guard** | `internal/ctxtokens` reads each live agent's context-window fill from its transcript and classifies it `ok`/`warning`/`critical`. The poller shows a state-colored token figure in `ls`/TUI/web, alerts once per upward crossing (`WARDEN_TOKEN_WARN_ALERT`), and auto-sends `/compact` at `critical` when the agent is idle (`WARDEN_TOKEN_AUTO_COMPACT`, cooldown-guarded). Master switch `WARDEN_TOKEN_GUARD`; thresholds `WARDEN_TOKEN_WARN`/`WARDEN_TOKEN_CRITICAL`. |

The web dashboard surfaces these as a **Resources panel** with live per-agent + system resource charts (uPlot).

```sh
warden stats           # one-shot view of the fleet's resource footprint
warden stats --watch   # redraw every 3s until interrupted
warden stats --json    # machine-readable
```
