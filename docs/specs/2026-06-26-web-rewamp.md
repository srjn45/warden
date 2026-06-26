# Web Mission Control — Rewamp (Overview declutter, URL routing, Metrics tab)

**Date:** 2026-06-26
**Status:** Proposed design (pre-implementation — awaiting review)
**Owner:** Srajan Pathak
**Branch / worktree:** `web-rewamp` (`.claude/worktrees/web-rewamp`)
**Scope:** `web/` (Astro + React SPA). One small daemon-side note (no required
change — see §6).

---

## 1. Problem

The current web UI (`web/src/components/Dashboard.tsx`) has grown a cluttered
**Overview** home screen and a flat client-only tab model that doesn't map to
URLs. Concretely:

1. **Overview is cluttered.** It stacks seven sections in one scroll:
   *Needs you*, *File conflicts*, *Fleet*, *Resources*, *Quick spawn*, *Recent
   activity*, and an *All agents* grid (`OverviewTab.tsx`). Resources/metrics,
   the redundant spawn widget, and a second full agent grid all compete for the
   same screen.
2. **Two ways to spawn.** Both the top-right `+ New agent` button
   (`AttentionBar.tsx`) and the *Quick spawn* card (`QuickSpawn.tsx`) create
   agents. The top-right button is enough.
3. **Two agent grids.** Overview's *All agents* mini-grid duplicates the
   Cockpit's full grid (`CockpitTab.tsx`) — same `AgentGrid` component, different
   `lines`.
4. **Tabs aren't URLs.** Active tab is React state persisted to
   `localStorage` (`web/src/lib/tabs.ts`); every tab is the same URL (`/`). No
   deep-linking, no back/forward, no shareable `/cockpit`.
5. **No dedicated metrics view.** Resource/footprint data is buried in an
   Overview card (`ResourcesPanel.tsx`) with a single attributed-RSS chart; there
   are no per-agent CPU / memory / context graphs, no fleet-size trend, no
   tokens-saved trend.

## 2. Goals

- **Declutter Overview** → repurpose it as the *Fleet*-centric landing surface,
  and make **Cockpit the default** view at `/`.
- **Move the Fleet summary into Cockpit** and drop the redundant *All agents*
  grid and *Quick spawn* widget.
- **Real URL routing** — `/cockpit`, `/pipelines`, `/context`, `/archive`,
  `/metrics`, and `/agent/<id>` — with `/` rendering Cockpit. Keep the existing
  tab bar UI, but tabs now navigate (push history) instead of flipping state.
- **New Metrics tab** with separate graphs: **CPU per agent**, **Memory per
  agent**, **Context per agent**, **number of agents**, **tokens saved**.
- Keep everything else (theme, notifications, shortcuts, attention bar, agent
  detail panes, batch ops) working.

## 3. Non-goals

- No backend/API redesign. The data needed already exists (§5.3). The only
  daemon consideration is SPA-fallback, which already works (§6).
- No visual rebrand / design-system overhaul. This is structural: fewer, clearer
  surfaces + routing + a metrics view.
- No new persisted server-side metrics. Context-per-agent history is derived
  client-side from existing live data (§5.3, item C) — we are **not** adding a
  context column to the metrics recorder in this pass (called out as a possible
  follow-up).

---

## 4. Proposed structure

### 4.1 Tab / route map

| Route        | Tab label              | Component             | Notes |
|--------------|------------------------|-----------------------|-------|
| `/`          | (Cockpit)              | `CockpitTab`          | Default — `/` renders Cockpit, canonical path stays `/` (no redirect) |
| `/cockpit`   | ⊞ Cockpit              | `CockpitTab`          | Same view as `/`; now hosts the Fleet summary header |
| `/pipelines` | ⛓ Pipelines           | `PipelinesTab`        | unchanged content |
| `/metrics`   | 📊 Metrics             | `MetricsTab` (**new**)| per-agent CPU/mem/context + fleet size + tokens saved |
| `/context`   | 🗒 Context & Messages  | `ContextMessagesTab`  | unchanged content |
| `/archive`   | 🗄 Archive             | `ArchiveTab`          | unchanged content |
| `/agent/<id>`| `<id>` (closeable)     | `AgentTab`            | pinned agent panes become real URLs |

**Removed surfaces:**
- The standalone **Overview** tab is removed from the tab bar. Its still-useful
  pieces (*Needs you* / attention queue, *File conflicts*) are relocated — see
  §4.3. The redundant *All agents* grid and *Quick spawn* card are deleted. The
  *Resources* card moves into the new **Metrics** tab.

> **Decision point for review:** Overview currently also hosts the *Needs you*
> attention queue and *File conflicts* panel. The request says "remove the all
> agents, move the Fleet to Cockpit" and "move metrics to a new tab" — it does
> not explicitly say where attention/conflicts go. Proposed: fold both into
> **Cockpit** as a collapsible header strip above the grid (they're fleet-wide
> situational awareness and belong next to the agents). Alternative: keep a slim
> Overview. **Flagging for your call (see §9 Q1).**

### 4.2 Cockpit becomes the home

`CockpitTab` gains a header region above the agent grid:

```
┌──────────────────────────────────────────────────────────┐
│ Fleet:  12 total · 4 busy · 2 waiting · 1 errored         │  ← FleetStats (moved here)
│ pressure: normal           dirs: warden(8) site(3) …      │
├──────────────────────────────────────────────────────────┤
│ ⚠ Needs you (2)   ▸ agent-x waiting · agent-y errored     │  ← AttentionQueue (relocated, collapsible)
│ ⚑ Conflicts (1)   ▸ api.go edited by agent-a, agent-b     │  ← ConflictsPanel (relocated, collapsible)
├──────────────────────────────────────────────────────────┤
│  [ agent grid — existing full-size tiles, lines=14 ]      │
└──────────────────────────────────────────────────────────┘
```

- `FleetStats` renders once, at the top of Cockpit (was in Overview).
- `AttentionQueue` + `ConflictsPanel` move here as compact, collapsible rows
  (default expanded only when non-empty) — pending the §9 Q1 decision.
- The grid, batch-select, bulk action bar, and per-pane `+` spawn are unchanged.

### 4.3 What moves where (component-level)

| Piece                | From (Overview)     | To                          |
|----------------------|---------------------|-----------------------------|
| `FleetStats`         | Overview card       | **Cockpit header**          |
| `AttentionQueue`     | Overview *Needs you*| **Cockpit header** (collapsible) |
| `ConflictsPanel`     | Overview *Conflicts*| **Cockpit header** (collapsible) |
| `ResourcesPanel`     | Overview *Resources*| **Metrics tab**             |
| `QuickSpawn`         | Overview            | **Deleted** (top-right `+ New agent` is the one spawn path) |
| `AgentGrid` (mini)   | Overview *All agents* | **Deleted** (Cockpit has the canonical grid) |
| `ActivityFeed`       | Overview *Recent activity* | **Deleted from Overview**; see §9 Q2 (move to Metrics/Cockpit or drop) |
| `OverviewTab.tsx`    | —                   | **Deleted** (after relocating its parts) |

### 4.4 New Metrics tab (`/metrics`)

A scrollable column of self-contained chart cards, each rendered with **uPlot**
(already a dependency, used by `ResourcesPanel`). Polls `getMetrics()` +
`getMetricsHistory()` every 5s (same cadence as today) and `getSavings()` (new
thin client, §5.3 E) on a slower 30s cadence.

Charts (each its own `<section className="card">`):

1. **CPU per agent** — multi-series line chart, one series per live agent,
   y = `cpu_percent`, x = time. Source: `getMetricsHistory()` →
   `sample.agents[].cpu_percent`.
2. **Memory per agent** — multi-series line chart, one series per agent,
   y = `rss_bytes` (GiB), x = time. Source: `sample.agents[].rss_bytes`.
3. **Context per agent** — per-agent context-token usage. Source: live
   `Session.context_tokens` (the metrics history has **no** context column). Two
   render options (§9 Q3): (a) a **live grouped bar chart** of current
   `context_tokens` per agent (simplest, always correct), or (b) a client-side
   accumulated time series built from SSE session updates while the tab is open
   (richer, but resets on reload). **Proposed: (a) bar chart now**, with a small
   `% of window` annotation from `context_state`.
4. **Number of agents** — single-series area/line, y = `system.agent_count`,
   x = time. Source: `getMetricsHistory()` → `sample.system.agent_count`.
5. **Tokens saved** — single-series bar/line of daily saved tokens. Source:
   `GET /savings?bucket=day` → `Summary.Buckets[].SavedTokens`, plus a headline
   number (`Summary.SavedTokens`, `SavedDollars`). Gated: if savings is disabled
   the daemon returns **403** — the card shows a friendly "enable `savings: true`
   in the config" message instead of an empty chart.

Each per-agent multi-series chart gets a stable color per agent id and a compact
legend. Empty/auth/disabled states render inline (no blank canvases).

---

## 5. Implementation plan

### 5.1 Routing layer (the core change)

The app is an Astro **static** SPA mounted once (`src/pages/index.astro` →
`<Dashboard client:only="react" />`). We introduce client-side routing without
adding a router dependency:

- **New `web/src/lib/router.ts`** — a tiny hash-free History-API helper:
  - `parseRoute(pathname): Route` → maps `/`→`{kind:'cockpit'}`,
    `/cockpit|/pipelines|/metrics|/context|/archive`→ fixed tab,
    `/agent/<id>`→`{kind:'agent', id}`, anything else → cockpit (default).
  - `routeToPath(route): string` → inverse, for building hrefs / `pushState`.
  - `navigate(route)` → `history.pushState` + dispatch an internal event.
  - `useRoute()` React hook → subscribes to `popstate` + our navigate event,
    returns the current `Route`. Replaces the `useReducer(tabsReducer, …)` +
    `localStorage` tab state in `Dashboard.tsx`.
  - Pinned agent tabs: keep the "pinned list" concept (so multiple agent tabs
    show in the bar) in `localStorage`, but **active** is derived from the URL,
    not stored. Opening an agent = `navigate('/agent/<id>')` + add to pinned;
    closing = remove from pinned + navigate back to `/cockpit`.
- **`Dashboard.tsx`** — replace tab reducer with `useRoute()`. The big
  `tabs.active === 'overview' && …` switch becomes a `switch (route.kind)`.
  Keyboard shortcuts (`j/k`, `1-9`, `/`) now call `navigate(...)` over the
  ordered route list instead of dispatching reducer actions.
- **`TabBar.tsx`** — render `<a href>` anchors (with `onClick`→`navigate`,
  `preventDefault`) so tabs are real links (middle-click / open-in-new-tab work)
  and highlight via `route` match. Drop the `Overview` button; add `Metrics`.
- **`lib/tabs.ts`** — refactor: `FIXED_TABS` becomes the route list
  `['cockpit','pipelines','metrics','context','archive']` (no `overview`).
  Keep `orderedTabs` / index / nav helpers but key them off routes. Pinned-agent
  persistence stays; active-tab persistence is removed (URL is the source of
  truth). Update `lib/tabs.test.ts` accordingly.

**Deep-link / refresh behavior:** loading `/metrics` directly must serve the
SPA. In **production** the daemon already does SPA fallback for unknown paths
(`internal/daemon/static.go` serves `index.html`) — ✅ no server change. In
**dev** (`astro dev`), direct loads of `/metrics` 404 because Astro is
file-routed. Fix: add a catch-all page **`web/src/pages/[...path].astro`** that
renders the same `<Dashboard>` — so both `/` and any sub-path hydrate the SPA in
dev and in the static build. (Astro emits these as static HTML shells; the React
app reads `location.pathname`.)

### 5.2 Component work

- **New** `web/src/components/MetricsTab.tsx` + per-chart subcomponents
  (or one component with N `useUplot` instances). Reuse the uPlot setup pattern
  from `ResourcesPanel.tsx` (theme-neutral `#888` axes). Factor the repeated
  uPlot create/update/destroy lifecycle into a small `web/src/lib/uplot.ts` (or a
  `useUplot` hook) to avoid copy-paste across 5 charts.
- **New** `web/src/lib/metricsSeries.ts` — pure transforms from
  `MetricsSample[]` → per-agent aligned series (CPU, RSS), fleet-size series, and
  from `Summary.Buckets` → tokens-saved series. Unit-tested (mirrors existing
  `lib/metrics.test.ts`).
- **Edit** `CockpitTab.tsx` — add the Fleet header (FleetStats) and the
  relocated AttentionQueue/ConflictsPanel strip.
- **Edit** `Dashboard.tsx` — routing, switch, drop Overview wiring, add Metrics.
- **Edit** `TabBar.tsx` — links + Metrics tab − Overview tab.
- **Delete** `OverviewTab.tsx`, `QuickSpawn.tsx` (and its test/usages),
  `QuickAddButton.tsx` if unused after this. Move `ResourcesPanel.tsx` usage to
  Metrics (keep the component, or fold into MetricsTab).
- **Keep** the top-right `+ New agent` button (`AttentionBar.tsx`) as the single
  spawn entry point — no change beyond it now being the only one.

### 5.3 Data sources (all already exist)

| Graph                 | Source (web client)                              | Backing route |
|-----------------------|--------------------------------------------------|---------------|
| A. CPU per agent      | `getMetricsHistory()` → `agents[].cpu_percent`   | `GET /metrics/history` |
| B. Memory per agent   | `getMetricsHistory()` → `agents[].rss_bytes`     | `GET /metrics/history` |
| C. Context per agent  | live `Session.context_tokens` / `context_state`  | `GET /sessions` + SSE (already in `Dashboard`) |
| D. Number of agents   | `getMetricsHistory()` → `system.agent_count`     | `GET /metrics/history` |
| E. Tokens saved       | **new** `getSavings()` client → `Buckets[]`      | `GET /savings?bucket=day` (exists) |

Only **one new client function** is needed: `getSavings(sinceISO?, bucket?)` in
`web/src/lib/api.ts` returning a `Summary` type mirrored in
`web/src/lib/savings.ts` (new, mirrors `internal/savings/savings.go` `Summary`).
Handle the 403 (disabled) path explicitly.

### 5.4 Tests

- Update `web/src/lib/tabs.test.ts` for the new route list (no `overview`,
  `+metrics`).
- New `web/src/lib/router.test.ts` — `parseRoute`/`routeToPath` round-trips,
  default-to-cockpit, agent routes.
- New `web/src/lib/metricsSeries.test.ts` — series transforms, empty input.
- Adjust any test that imports the deleted `OverviewTab`/`QuickSpawn`.
- `npm run test` (vitest) green; `npm run build` (astro) green.

---

## 6. Daemon / server impact

**None required.** `internal/daemon/static.go` already serves `index.html` for
any non-API, non-file GET (SPA fallback), so `/cockpit`, `/metrics`,
`/agent/<id>` deep-links and refreshes resolve to the app. The `/savings`,
`/metrics`, `/metrics/history` routes already exist and are auth-gated like the
rest. No new endpoints, no route changes.

(One optional future follow-up, explicitly **out of scope**: add a context-token
column to the metrics recorder so *Context per agent* can be a true historical
time series instead of a live snapshot. Tracked, not done here.)

## 7. Risks / edge cases

- **Per-agent series churn:** agents come and go; CPU/memory line charts must
  handle series appearing/disappearing across the history window without
  crashing uPlot (rebuild series defs when the agent set changes).
- **localStorage migration:** existing users have `warden.tabs` with
  `active:'overview'`. The new loader ignores stored `active` (URL wins) and
  only reads `pinned`; an old blob must not break parsing.
- **Pinned-tab default route:** closing the last agent tab or landing on a stale
  `/agent/<deadid>` must fall back to `/cockpit` (mirror today's prune→overview
  behavior, retargeted to cockpit).
- **Savings disabled (403):** Metrics tab must degrade gracefully, not error.
- **Dev vs prod routing parity:** the `[...path].astro` catch-all keeps `astro
  dev` and the static build behaving like the daemon's fallback.

## 8. Definition-of-Done checklist (per CLAUDE.md)

To be completed at delivery (after plan approval + implementation):

- [ ] **Tag & release** — `minor` bump (sizable feature). Confirm before pushing
      the `v*` tag (it cuts the public release).
- [ ] **Docs** — `README.md` web section; `docs/FEATURES.md` + root `FEATURES.md`
      matrix (Metrics tab, URL routing); `docs/USAGE.md` if it covers the web UI.
- [ ] **Website** — `site/src/content/docs/guides/web-mission-control.*` (new
      Metrics view, routing) and any `reference/` page that lists web surfaces.
- [ ] **Skill** — `skills/warden/` only if agent-facing guidance changes (likely
      n/a — this is human UI).
- [ ] **CLI help** — n/a (no cobra command changes).

## 9. Open questions for review (please decide before I implement)

**Q1 — Attention queue & conflicts placement.** With Overview gone, where do
*Needs you* (attention queue) and *File conflicts* live? Proposed: collapsible
strip at the top of **Cockpit**. Alternatives: a slim retained **Overview** tab,
or surface them only via the existing top-bar `⚠ N needs you` pill.

**Q2 — Recent activity feed.** Overview's `ActivityFeed` has no obvious new home.
Proposed: **drop it from the home view** (the per-agent panes already show
events). Alternative: add it as a card on the Metrics tab or Cockpit footer.

**Q3 — Context-per-agent graph shape.** Proposed: **live grouped bar chart** of
current `context_tokens` per agent (no historical store needed). Alternative:
client-side accumulated time series (richer, resets on reload), or defer a true
historical series to the daemon follow-up in §6.

**Q4 — `/` canonical path.** Proposed: `/` renders Cockpit and **stays** `/`
(no redirect to `/cockpit`); the Cockpit tab links to `/cockpit`. Both show the
same view. Alternative: redirect `/` → `/cockpit` so there's a single canonical
URL.
