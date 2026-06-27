---
title: Web mission control
description: The embedded browser dashboard — URL-routed shell, Cockpit home, a Metrics view, live SSE fleet, and interactive terminals.
---

The daemon embeds a React dashboard (Astro + React) and serves it at `http://localhost:8765` alongside the REST API — no separate server required.

```sh
warden daemon
open http://localhost:8765
```

![The warden web dashboard on its Cockpit home: a routed mission-control shell with a Fleet summary header (totals · busy · waiting · errored · pressure) above the agent grid, grouped by directory, with Pipelines, Metrics, Archive, and Others tabs alongside.](/warden/media/web-cockpit.png)

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
| `/tui` | *(top-bar ▢ TUI button)* | The **full `warden tui`, streamed full-screen** — not a tab; launched from the highlighted top-bar button and exited with **Ctrl+Q**. See [Full-screen TUI](#full-screen-tui). |

`/` redirects to `/cockpit`, so there is a single canonical home URL. Deep-linking and refresh work because the daemon serves the SPA for any non-API path.

In the Cockpit you can **group the fleet** by directory, task type, status, or tag to keep a large fleet legible:

![The Cockpit grouped by task type: agents collected under collapsible *analysis* and *development* sections, each with a per-group count.](/warden/media/web-cockpit-grouped.png)

## What it does

| Feature | Description |
|---|---|
| **URL routing** | Real History-API routes (above) — back/forward, refresh, middle-click-open-in-new-tab, and shareable deep links all work. |
| **Cockpit home** | The default view: a **Fleet** summary header above the canonical agent grid. The old *Quick spawn* widget and duplicate *All agents* mini-grid were removed. |
| **Live fleet over SSE** | No manual refresh; coloured busy/idle badges (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) + each agent's subject. |
| **Attention queue** | In the **Others** tab: surfaces agents in `waiting_for_input`/`errored`/`orphaned`, with one-click approval buttons. |
| **Metrics view** | A dedicated `/metrics` tab — see [Metrics view](#metrics-view). |
| **Context & Messages overlay** | Opened from a small **🗒 header button** as a dismissible overlay (Esc closes); it's no longer a tab. |
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. **Mobile-friendly:** swipe to scroll back through history (the swipe drives tmux/the agent's scrollback the same way a mouse wheel does), plus a sticky on-screen key bar (Esc, Tab, Ctrl-C, ↑/↓, jump-to-bottom) for the keys a phone keyboard lacks. The layout tracks the visible viewport, so the bottom tab nav and the key bar stay put above the soft keyboard. |
| **Full-screen TUI** | The whole `warden tui` in the browser, launched from the top bar — see [Full-screen TUI](#full-screen-tui). |
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

## Full-screen TUI

The highlighted **▢ TUI** button in the top bar opens the full terminal cockpit
— the literal `warden tui` (`/tui`) — **streamed full-screen into the browser**,
so you can drive the entire fleet from a laptop exactly as you do locally. It
isn't a tab: it takes the **whole viewport, edge-to-edge and non-scrollable**,
with none of the dashboard chrome, and you **exit with Ctrl+Q** from any pane
(which lands you back on the home view).

It isn't a reimplementation. The daemon builds a shared three-pane tmux cockpit
(agent **list** pane + a **master** shell/REPL pane + a **detail** pane that opens
the selected agent) and bridges it to an xterm.js terminal over the same
WebSocket PTY that powers a per-agent attach. Because it's the real session:

- **Every TUI keybinding works** — `enter` open · `n` new · `o` dir · `s` send ·
  `a` attach · `i` info · `x` kill · `r` refresh · `?` help · `j`/`k`, `g`/`G`,
  **Alt+Arrow** to move between panes, and the rest — they reach the cockpit
  unchanged. (**Ctrl+Q** is the one chord held back by the browser, to exit.)
- **The shells are real shells.** The master pane runs *your* `$SHELL` with your
  rc files, so tab-completion, autosuggestions, history, and fzf all behave as
  they do locally — nothing is emulated, only piped.
- **Claude Code in the detail pane is the real Claude Code**, with its full
  interactive UI. Shift+Enter is mapped to its newline.
- **Shared across clients.** There's one web cockpit; the most-recently-active
  client drives the window size.

It's built lazily on first attach and reused after that. While the terminal has
focus the dashboard's global single-key shortcuts stay dormant (keys flow to the
TUI). The one place a browser can't match a local terminal is a handful of
browser-reserved chords (`Ctrl+T`/`Ctrl+W`/`Ctrl+N`) — installing the dashboard
as a PWA reclaims most of them. This is a **desktop/laptop surface** — the
three-pane cockpit wants width, so there's no mobile key bar; on a phone, pin a
single agent (above) instead.

## Metrics view

![The Metrics tab, upper half: CPU per agent beside Total CPU, Memory per agent beside Total memory, and a Cost-per-agent card (total · today · this week, with per-agent input/output token counts).](/warden/media/web-metrics.png)

![The Metrics tab, lower half: Number of agents, the Tokens-saved trend with a 24h/48h/7d/30d/All window picker and a cumulative line, a Savings-by-feature stacked area (compact / llm_offload / check / commit), and the Live-footprint resource chart.](/warden/media/web-metrics-2.png)

The **Metrics** tab (`/metrics`) is a responsive grid of self-contained uPlot chart cards — **two columns** on wide screens (each per-agent chart sits beside its fleet-wide total), collapsing to a **single column** on phones:

| Card | What it shows |
|---|---|
| **CPU per agent** | One line series per live agent, `cpu_percent` over time (from the metrics history store). |
| **Total CPU** | A single fleet-wide line: `cpu_percent` summed across all agents per sample. Sits beside *CPU per agent*. |
| **Memory per agent** | One line series per agent, resident memory in GiB over time. |
| **Total memory** | A single fleet-wide line: resident memory (GiB) summed across all agents. Sits beside *Memory per agent*. |
| **Cost per agent** | A **total · today · this week** dollar headline over a per-agent table — each agent's measured `$` cost with its input/output token counts — from the [cost-governance](/warden/reference/savings/#cost-governance--warden-spend--the-budget-gate) rollup (`warden spend`). |
| **Context per agent** | One line series per agent of its live context-window fill over time, with the legend dot coloured by `ok`/`warning`/`critical`. This series is **accumulated client-side** (a ring buffer over the live SSE feed) — it survives tab switches but **starts fresh on a full page reload** (a persisted history is a tracked daemon follow-up). |
| **Number of agents** | Fleet size (`agent_count`) over time. |
| **Tokens saved** | The saved-tokens trend (from the [savings ledger](/warden/reference/savings/)) with a **window picker** — `24h`/`48h` bucket by **hour**, `7d`/`30d`/`All` by **day** — so a fresh ledger still plots a real curve instead of a single point. A filled area shows tokens saved per bucket against a left axis, and a dashed line shows the **running cumulative** against the right axis, plus a headline saved-tokens / dollars figure. The trend is zero-filled, so idle intervals read as real zeros rather than gaps. If the ledger is disabled (`savings: false`) the card shows a "set `savings: true`" hint instead of an empty chart. |
| **Savings by feature** | A per-feature **stacked-area** breakdown of the same trend — which lifecycle feature (`llm_offload`, `commit`, `check`, `compact`) drove the savings — over the selected window. |
| **Live footprint** | The former Resources panel — live per-agent + system resource charts. |

## Build & run

```sh
make release     # 1. builds the Astro UI (web/), 2. embeds it via go:embed, 3. builds bin/warden
warden daemon    # start the daemon as usual
```

Then open `http://localhost:8765` in a browser.

> The UI is baked into the binary at build time. After changing anything under `web/`, rebuild (`make release`, or `make ui` for the frontend only) and restart the daemon. For live UI iteration, run `warden daemon` and `make ui-dev` in parallel and open `http://localhost:4321` — the Astro dev server proxies `/api/*` (the whole REST surface, incl. the `/api/v1/sessions/{id}/attach` and `/api/v1/cockpit/attach` WebSockets and `/api/v1/events/stream` SSE) and `/healthz` to `:8765`, so SSE and all REST calls work without CORS configuration.
