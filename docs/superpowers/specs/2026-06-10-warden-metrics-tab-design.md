# Warden Metrics Tab — Design Spec

**Date:** 2026-06-10  
**Status:** Future enhancement — high-level design. Detail pass required before implementation.

---

## Summary

Promote observability to a first-class, dedicated Metrics tab in the web UI. Simultaneously clean up the Overview tab by removing the panels that now have their own homes (agents → Cockpit/agent tabs, resources → Metrics tab).

---

## Overview Tab Cleanup

Remove from Overview:
- "All Agents" list (agents have Cockpit + individual tabs)
- `ResourcesPanel` (moving to Metrics tab)

Overview becomes: `FleetStats` (agent counts by status) + `AttentionQueue` + `ActivityFeed` (recent events). A clean dashboard — entry point for attention, not data.

---

## Metrics Tab

### Graphs

Four separate `uPlot` time-series charts in a responsive grid (2×2 on desktop, stacked on mobile):

| Graph | Signals | Threshold lines |
|---|---|---|
| **Memory** | System total/used + attributed RSS per agent, wired, compressed bytes | warn / critical bands |
| **CPU** | Per-agent CPU% (stacked) + system total | warn / critical |
| **Processes** | Per-agent proc count over time | — |
| **Token Usage** | Per-agent context token count over time | warn / critical (mirrors context-size guard) |

Below the graphs: a live snapshot table — one row per agent, all four signals at the current moment. Sortable by any column.

### Thresholds

Configurable via env vars with sensible defaults:

```
WARDEN_MEM_WARN=70       # % of total RAM
WARDEN_MEM_CRIT=90
WARDEN_CPU_WARN=80
WARDEN_CPU_CRIT=95
WARDEN_TOKEN_WARN=70     # % of context window (mirrors context-size guard)
WARDEN_TOKEN_CRIT=90
```

Rendered as horizontal dashed lines on each graph. The daemon emits a threshold breach event into the agent's event log so it shows up in the ActivityFeed too.

---

## Token Data Plumbing

Token tracking currently lives in the store (`UpdateContext` → state band green/orange/red). To chart it as a time-series alongside memory/CPU, add `ContextTokens int` to `AgentStat` in `internal/metrics/types.go`. The daemon populates this from the store at sample time. Token history then flows through the existing `/metrics/history` JSONL recorder — no new endpoints.

Open question for detail pass: should token data sampling align with the metrics 15s interval, or is the store's event-driven approach (only on state change) sufficient for a chart?

---

## Additional Observability Ideas

These are candidate additions for the Metrics tab or as separate panels — to be prioritized separately:

- **Token velocity** — tokens consumed per minute per agent. Highlights runaway context growth before it hits the critical threshold. Derived from the token time-series; no new data collection.
- **Compact event overlays** — vertical marker lines on the token graph showing when `/compact` was triggered on an agent. Sourced from store events.
- **Daemon health chart** — goroutine count + open FD count over time (already in `DaemonStat`). Small sparkline in the Metrics tab footer.
- **Pipeline throughput panel** — jobs completed/failed per pipeline + p50/p95 job duration. Cross-links to the Pipelines tab. Requires pipeline job timing data in the store (currently not tracked).
- **Error rate** — tool failures per agent per hour (derived from transcript parsing, similar to digest). Low priority.

---

## What Stays Unchanged

- `/metrics` and `/metrics/history` endpoints — same shape, just extended with `ContextTokens`
- 15-second recorder interval — adequate for these signals
- `uPlot` library — already in use, no new charting dependency

---

## Open Questions for Detail Pass

1. History retention — currently unbounded JSONL. Add a rolling window (e.g., 24h) or size cap?
2. Per-agent color assignment — needs a stable palette so the same agent is the same color across all four graphs.
3. Threshold config via UI? Or env-var only for v1?
4. Mobile layout — 2×2 grid won't fit on small screens; stacked single-column graphs need a height budget.
