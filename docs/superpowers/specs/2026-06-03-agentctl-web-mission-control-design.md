# agentctl Web — Mission Control redesign

**Date:** 2026-06-03
**Status:** Approved (brainstorm) — ready for implementation plan

## Goal

Mature the web interface to match (and exceed) the TUI, leaning into capabilities a
terminal can't offer. The web app becomes a "mission control" dashboard built around
three superpowers chosen with the user:

1. **Multi-pane cockpit** — watch many agents' live output at once.
2. **Live terminal streaming** — real, colored terminal output streamed in near-real-time.
3. **Attention queue + notifications** — surface "who needs you now" and ping the browser.

The git-diff/changes viewer was explicitly **out of scope** for this pass.

The chosen layout is a **tabbed shell** (option C in brainstorming): fixed `Overview` and
`Cockpit` tabs plus per-agent tabs the user pins open. This was selected for
extensibility — new surfaces (logs, diffs, a New-agent tab) can be added later without a
redesign.

## Architecture & scope of change

**Kept as-is:** Astro + React; the session-list SSE stream (`GET /events/stream`); all
lifecycle endpoints (`/spawn`, `/sessions/{id}/input`, `/sessions/{id}/terminate`,
`/sessions/{id}/delete`, `/sessions/{id}/remove-worktree`, `/fs/dirs`); the cheap plain
snapshot endpoint `GET /sessions/{id}/output?lines=N`.

**One new backend endpoint:** `GET /sessions/{id}/output/stream` — Server-Sent Events.
Pushes `tmux capture-pane -e` snapshots on a ~1s cadence. The `-e` flag preserves ANSI
escape sequences so the client renders **colored** output. This is a new streaming
variant; the existing plain `Output()` path (no `-e`) is left untouched and continues to
back the mini-tiles.

**One new frontend dependency:** `xterm.js` — a terminal emulator for the focused agent
terminal. It handles ANSI color, line wrapping, and scrollback. Each ~1s frame does a
terminal reset + write of the current pane snapshot, which matches exactly what the TUI
displays (tmux `capture-pane` returns the current visible screen, not a growing log, so
snapshot-replace is the correct model).

The only new server surface is the output SSE stream; everything else is frontend work.

## The tabbed shell

```
┌─ agentctl ──── live ── ⚠ 2 need you ──────── 🔔 notify ─ + New ─┐  ← AttentionBar (always visible)
├─[ Overview ][ ⊞ Cockpit ][ A-1 ● ][ B-2 ⚠ ✕ ]──────────────────┤  ← TabBar
│                                                                  │
│                      active tab content                          │
└──────────────────────────────────────────────────────────────────┘
```

- **AttentionBar** (always visible, top): live/reconnecting indicator; count of agents
  needing attention (click → jump to Overview); notifications toggle; **+ New**.
- **TabBar**: `Overview` and `Cockpit` are permanent. Clicking an agent anywhere (a queue
  card, a mini-tile, a cockpit pane) pins it as a closeable agent tab. Open tabs and the
  active tab persist to `localStorage`, so a reload restores the user's workspace.

## Tab contents

### Overview tab (all four sections)

- **Attention queue** — cards for agents in `waiting_for_input` / `errored`, including the
  prompt/subject excerpt. Click a card to pin + focus that agent.
- **Fleet stats** — counters (total / busy / waiting / errored), grouped by directory; a
  health summary of the whole fleet.
- **All-agents mini-grid** — live thumbnail tiles for every agent (last ~8 lines), polled
  every ~2s via the existing plain `/output` endpoint. Click a tile to pin its tab.
- **Recent activity feed** — a merged, reverse-chronological stream of events across all
  agents (spawned, tool calls, completed, errored).
- **Quick spawn** — inline prompt textarea + directory picker (reuses the existing
  `DirPicker`), so launching is one step from the home screen.

### Cockpit tab

The mini-grid rendered at full size, grouped by directory (mirrors the TUI cockpit).
Click any pane to pin + focus that agent's tab.

### Agent tab (one per pinned agent)

- Full **xterm.js** terminal fed by `GET /sessions/{id}/output/stream`.
- Send-input box (`POST /sessions/{id}/input`).
- Collapsible details: type, directory, prompt; plus the event timeline.
- The reworked terminate controls (see below).

## Live output strategy

Two tiers, to balance liveness against connection load on a local single-user daemon:

- **Mini-tiles** (Overview grid + Cockpit panes): cheap. Last ~8 lines from the existing
  plain `GET /sessions/{id}/output?lines=N`, polled ~2s. A glance, not a live feed; no new
  SSE fan-out.
- **Focused agent terminal** (pinned tab): smooth. `GET /sessions/{id}/output/stream` SSE
  at ~1s, rendered through xterm.js with reset+write per frame for full color.

## Notifications

A `useNotifications` hook subscribes to the session-list SSE and detects transitions
**into `waiting_for_input`**. When such a transition occurs **and `document.hidden` is
true** (the tab is not focused), it fires a browser `Notification`. Clicking the
notification focuses the window and pins the relevant agent's tab.

Permission is requested via the top-bar 🔔 toggle (a user gesture, which browsers require
for `Notification.requestPermission()`); notifications are off until the user enables them.

Per the chosen scope, the only trigger is `waiting_for_input`, gated on tab-hidden. No
notifications for `done` or `errored` transitions.

## Fixing the broken Terminate

The current `TerminateControls` component POSTs to `/cleanup`, an endpoint that **does not
exist** in the daemon router — so Terminate is non-functional in the web UI today. It is
rewired to the real endpoints, preserving the existing guard→force UX:

- **Terminate** → `POST /sessions/{id}/terminate` (stops the agent; marks status `done`).
- If the agent has a **worktree**, offer **Remove worktree** → `POST
  /sessions/{id}/remove-worktree`. On a `409` guard (dirty / unpushed / agent-alive),
  surface the returned message and offer a **Force** retry (`{force: true}`).
- **Hard-delete record** → `POST /sessions/{id}/delete {hard: true}` (vs. the default
  archive on an empty body).

## Component & file layout

```
web/src/lib/
  tabs.ts        reducer: open / close / activate / persist (localStorage) tabs
  notify.ts      permission handling + waiting_for_input transition trigger (hidden-gated)
  stats.ts       deriveFleetStats(sessions) -> counters grouped by dir
  activity.ts    mergeEvents(sessions) -> sorted activity feed
  api.ts         (extend) drop /cleanup; add terminate/removeWorktree/delete mappings;
                 add subscribeOutput(id) SSE helper
web/src/components/
  AttentionBar.tsx   TabBar.tsx
  OverviewTab.tsx    CockpitTab.tsx    AgentTab.tsx
  AttentionQueue.tsx FleetStats.tsx    AgentGrid.tsx    MiniTerminal.tsx
  ActivityFeed.tsx   QuickSpawn.tsx    Terminal.tsx (xterm wrapper)
  (reuse: DirPicker, BusyIdleBadge, EventTimeline, NewAgentModal-as-QuickSpawn,
   TerminateControls — reworked)
```

`Dashboard.tsx` becomes the thin shell hosting `AttentionBar` + `TabBar` + the active tab.

## Testing

Follows existing patterns (`web/src/lib/api.test.ts`, `internal/daemon/lifecycle_routes_test.go`,
`internal/daemon/sse_test.go`):

- **Vitest unit:** `tabs` reducer (open/close/activate/persist); `notify` trigger logic
  (only on transition into `waiting_for_input`, only when `document.hidden`);
  `stats`/`activity` derivations; the reworked `api` terminate/remove-worktree/delete
  mappings (correct URL + body per action).
- **Vitest + jsdom component:** TabBar open/close/persist; AttentionQueue rendering;
  AgentGrid tile → pin; OverviewTab section composition.
- **Go:** table-driven test for the new `GET /sessions/{id}/output/stream` SSE handler with
  a fake lifecycle, mirroring `sse_test.go` (headers, framing, shutdown drain).
- xterm rendering itself is not unit-tested (canvas/DOM heavy); the `Terminal` wrapper's
  feed/reset logic is tested in isolation.

## Out of scope

- Git diff / worktree changes viewer.
- Notifications for `done` / `errored`.
- True PTY/pipe-pane incremental streaming (snapshot-replace is sufficient and matches the
  tmux screen model).
- Authentication / multi-user (remains a local single-user tool).
