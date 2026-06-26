---
title: Web mission control
description: The embedded browser dashboard — URL-routed shell, Cockpit home, a Metrics view, live SSE fleet, and interactive terminals.
---

The daemon embeds a React dashboard (Astro + React) and serves it at `http://localhost:8765` alongside the REST API — no separate server required.

```sh
warden daemon
open http://localhost:8765
```

![The warden web dashboard: a routed mission-control shell with a Cockpit home, an Others catch-all, Pipelines, a Metrics view, and an Archive tab.](/warden/media/web-overview.png)

## Routes & tabs

The dashboard is a **URL-routed mission-control shell**. Tabs are **real URLs** (History-API routing — back/forward, refresh, and shareable deep links all work), not client-only state. Each surface has its own address:

| Route | Tab | What's there |
|---|---|---|
| `/cockpit` | ⊞ **Cockpit** | **The home view** (`/` redirects here). A slim **Fleet** header — totals · busy · waiting · errored, pressure, per-directory counts — above the full agent grid. |
| `/pipelines` | ⛓ **Pipelines** | Pipeline list + live DAG / per-job drawer. |
| `/metrics` | 📊 **Metrics** | Per-agent and fleet-wide charts — [see below](#metrics-view). |
| `/archive` | 🗄 **Archive** | Ended sessions with since/type filters. |
| `/others` | ▦ **Others** | The former *Overview*, renamed to a **catch-all** (sits last): *Needs you* (attention queue), *File conflicts*, and *Recent activity*. New/not-yet-homed widgets land here first. |
| `/agent/<id>` | `<id>` | A pinned agent's live terminal (one closeable tab per pinned agent). |

`/` redirects to `/cockpit`, so there is a single canonical home URL. Deep-linking and refresh work because the daemon serves the SPA for any non-API path.

## What it does

| Feature | Description |
|---|---|
| **URL routing** | Real History-API routes (above) — back/forward, refresh, middle-click-open-in-new-tab, and shareable deep links all work. |
| **Cockpit home** | The default view: a **Fleet** summary header above the canonical agent grid. The old *Quick spawn* widget and duplicate *All agents* mini-grid were removed. |
| **Live fleet over SSE** | No manual refresh; coloured busy/idle badges (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) + each agent's subject. |
| **Attention queue** | In the **Others** tab: surfaces agents in `waiting_for_input`/`errored`/`orphaned`, with one-click approval buttons. |
| **Metrics view** | A dedicated `/metrics` tab — see [Metrics view](#metrics-view). |
| **Context & Messages overlay** | Opened from a small **🗒 header button** as a dismissible overlay (Esc closes); it's no longer a tab. |
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. |
| **Create agent** | **+ New agent** prompt box with a directory picker (live prefix autocomplete) and a **Supervised** checkbox. |
| **Terminate with git guard** | Surfaces a 409 → **Force** + optional hard-delete when there's uncommitted/unpushed work. |
| **Digest panel** | View an agent's completion digest in the browser. |
| **Browser notifications** | Opt-in desktop notification when an agent enters `waiting_for_input` (gated to hidden tabs). |
| **Group the fleet** | In the Cockpit, group agents by directory, task type, status, or tag to keep a large fleet legible. |
| **Batch operations** | Select multiple agents and act on them at once (e.g. terminate / delete). |
| **Search & archive** | Full-text search across the fleet; browse archived (closed) agents from the Archive tab. |
| **Theme toggle** | Light / dark / system theming. |
| **Keyboard shortcuts** | Global shortcuts for navigation and actions (`1`–`9` jump to a tab, `j`/`k` next/previous, `Esc` close), with a `?` help overlay listing them. |

The web dashboard also has a **Pipelines** tab: it lists pipelines, shows a selected pipeline's jobs as status-colored cards with dependency chips, and a per-job drawer with the prompt/handoff/output, a **Cancel** (pipeline) / **Retry** (job) control, and an **Open terminal** link to a running job's session. (Creating / editing pipelines in the browser is not yet available — use `warden pipeline create -f`.)

## Metrics view

The **Metrics** tab (`/metrics`) is a responsive grid of self-contained uPlot chart cards — **two columns** on wide screens (each per-agent chart sits beside its fleet-wide total), collapsing to a **single column** on phones:

| Card | What it shows |
|---|---|
| **CPU per agent** | One line series per live agent, `cpu_percent` over time (from the metrics history store). |
| **Total CPU** | A single fleet-wide line: `cpu_percent` summed across all agents per sample. Sits beside *CPU per agent*. |
| **Memory per agent** | One line series per agent, resident memory in GiB over time. |
| **Total memory** | A single fleet-wide line: resident memory (GiB) summed across all agents. Sits beside *Memory per agent*. |
| **Context per agent** | One line series per agent of its live context-window fill over time, with the legend dot coloured by `ok`/`warning`/`critical`. This series is **accumulated client-side** (a ring buffer over the live SSE feed) — it survives tab switches but **starts fresh on a full page reload** (a persisted history is a tracked daemon follow-up). |
| **Number of agents** | Fleet size (`agent_count`) over time. |
| **Tokens saved** | Daily bars of tokens kept out of agents' context (from the [savings ledger](/warden/reference/savings/)), plus a headline saved-tokens / dollars figure. If the ledger is disabled (`savings: false`) the card shows a "set `savings: true`" hint instead of an empty chart. |
| **Live footprint** | The former Resources panel — live per-agent + system resource charts. |

## Build & run

```sh
make release     # 1. builds the Astro UI (web/), 2. embeds it via go:embed, 3. builds bin/warden
warden daemon    # start the daemon as usual
```

Then open `http://localhost:8765` in a browser.

> The UI is baked into the binary at build time. After changing anything under `web/`, rebuild (`make release`, or `make ui` for the frontend only) and restart the daemon. For live UI iteration, run `warden daemon` and `make ui-dev` in parallel and open `http://localhost:4321` — the Astro dev server proxies `/api/*` (the whole REST surface, incl. the `/api/v1/sessions/{id}/attach` WebSocket and `/api/v1/events/stream` SSE) and `/healthz` to `:8765`, so SSE and all REST calls work without CORS configuration.
