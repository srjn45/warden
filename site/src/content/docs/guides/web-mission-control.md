---
title: Web mission control
description: The embedded browser dashboard — tabbed shell, live SSE fleet, interactive terminals, and an attention queue.
---

The daemon embeds a React dashboard (Astro + React) and serves it at `http://localhost:8765` alongside the REST API — no separate server required.

```sh
warden daemon
open http://localhost:8765
```

![The warden web dashboard Overview tab: a "Needs you" attention queue with one-click approval buttons, a fleet summary, live per-agent resource metrics, a quick-spawn box, and a merged activity feed.](/warden/media/web-overview.png)

## What it does

The dashboard is a **tabbed mission-control shell**: two fixed tabs — **Overview** and **Cockpit** — plus one closeable tab per agent you pin.

| Feature | Description |
|---|---|
| **Tabbed mission-control shell** | Fixed **Overview** and **Cockpit** tabs, plus one closeable tab per pinned agent. |
| **Live fleet over SSE** | No manual refresh; coloured busy/idle badges (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) + each agent's subject. |
| **Attention queue** | Surfaces agents in `waiting_for_input`/`errored`/`orphaned`, with one-click approval buttons. |
| **Cockpit tab** | Multi-pane view for watching several agents at once. |
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. |
| **Create agent** | **+ New agent** prompt box with a directory picker (live prefix autocomplete) and a **Supervised** checkbox. |
| **Terminate with git guard** | Surfaces a 409 → **Force** + optional hard-delete when there's uncommitted/unpushed work. |
| **Digest panel** | View an agent's completion digest in the browser. |
| **Resources panel** | Live per-agent + system resource charts (uPlot). |
| **Browser notifications** | Opt-in desktop notification when an agent enters `waiting_for_input` (gated to hidden tabs). |
| **Group the fleet** | In the Cockpit, group agents by directory, task type, status, or tag to keep a large fleet legible. |
| **Batch operations** | Select multiple agents and act on them at once (e.g. terminate / delete). |
| **Search & archive** | Full-text search across the fleet; browse archived (closed) agents from the history view. |
| **Timeline** | A merged activity feed of agent lifecycle events. |
| **Theme toggle** | Light / dark / system theming. |
| **Keyboard shortcuts** | Global shortcuts for navigation and actions, with a `?` help overlay listing them. |

The web dashboard also has a **Pipelines** tab: it lists pipelines, shows a selected pipeline's jobs as status-colored cards with dependency chips, and a per-job drawer with the prompt/handoff/output, a **Cancel** (pipeline) / **Retry** (job) control, and an **Open terminal** link to a running job's session. (Creating / editing pipelines in the browser is not yet available — use `warden pipeline create -f`.)

## Build & run

```sh
make release     # 1. builds the Astro UI (web/), 2. embeds it via go:embed, 3. builds bin/warden
warden daemon    # start the daemon as usual
```

Then open `http://localhost:8765` in a browser.

> The UI is baked into the binary at build time. After changing anything under `web/`, rebuild (`make release`, or `make ui` for the frontend only) and restart the daemon. For live UI iteration, run `warden daemon` and `make ui-dev` in parallel and open `http://localhost:4321` — the Astro dev server proxies `/sessions` (incl. the `/attach` WebSocket), `/spawn`, `/events`, and `/healthz` to `:8765`, so SSE and all REST calls work without CORS configuration.
