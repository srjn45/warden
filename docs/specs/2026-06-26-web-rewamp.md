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
| `/`          | →                      | (redirect)            | **Redirects to `/cockpit`** — single canonical URL |
| `/cockpit`   | ⊞ Cockpit              | `CockpitTab`          | Default view; now hosts the Fleet summary header |
| `/others`    | ▦ Others               | `OthersTab` (renamed) | The former *Overview*, renamed — the catch-all landing spot for anything not yet homed |
| `/pipelines` | ⛓ Pipelines           | `PipelinesTab`        | unchanged content |
| `/metrics`   | 📊 Metrics             | `MetricsTab` (**new**)| per-agent CPU/mem/context + fleet size + tokens saved |
| `/archive`   | 🗄 Archive             | `ArchiveTab`          | unchanged content |
| `/agent/<id>`| `<id>` (closeable)     | `AgentTab`            | pinned agent panes become real URLs |
| _(no tab)_   | 🗒 header button       | `ContextMessagesTab`  | **removed from the tab bar** — opened from a small header button as an overlay (§4.5) |

**Decisions (resolved with maintainer):**
- **Overview is renamed to "Others"** (route `/others`), not deleted. It is the
  designated **catch-all** tab: the home for anything we haven't found a proper
  place for yet. New/orphaned widgets land here until they earn a dedicated home.
- *Needs you* (attention queue), *File conflicts*, and *Recent activity* **stay
  in Others**.
- Only the **Fleet** summary moves out of Others → into **Cockpit** (§4.2). The
  *All agents* grid and *Quick spawn* card are **deleted**; the *Resources* card
  moves into the new **Metrics** tab.

### 4.2 Cockpit becomes the home

`CockpitTab` gains a slim Fleet header above the agent grid:

```
┌──────────────────────────────────────────────────────────┐
│ Fleet:  12 total · 4 busy · 2 waiting · 1 errored         │  ← FleetStats (moved here)
│ pressure: normal           dirs: warden(8) site(3) …      │
├──────────────────────────────────────────────────────────┤
│  [ agent grid — existing full-size tiles, lines=14 ]      │
└──────────────────────────────────────────────────────────┘
```

- `FleetStats` renders once, at the top of Cockpit (was in Overview). This is
  the only piece that leaves Others for Cockpit.
- *Needs you* / *Conflicts* / *Recent activity* stay in the **Others** tab — not
  duplicated here.
- The grid, batch-select, bulk action bar, and per-pane `+` spawn are unchanged.

### 4.3 What moves where (component-level)

| Piece                | From (Overview)     | To                          |
|----------------------|---------------------|-----------------------------|
| `FleetStats`         | Overview card       | **Cockpit header** (the only piece that leaves) |
| `AttentionQueue`     | Overview *Needs you*| **Stays — Others tab**      |
| `ConflictsPanel`     | Overview *Conflicts*| **Stays — Others tab**      |
| `ActivityFeed`       | Overview *Recent activity* | **Stays — Others tab** |
| `ResourcesPanel`     | Overview *Resources*| **Metrics tab**             |
| `QuickSpawn`         | Overview            | **Deleted** (top-right `+ New agent` is the one spawn path) |
| `AgentGrid` (mini)   | Overview *All agents* | **Deleted** (Cockpit has the canonical grid) |
| `OverviewTab.tsx`    | —                   | **Renamed → `OthersTab.tsx`** (drops Fleet/QuickSpawn/All-agents; keeps Needs-you, Conflicts, Recent activity) |

After the rewamp the **Others** tab holds: *Needs you* (attention queue), *File
conflicts*, *Recent activity* — and is where any future not-yet-homed widget
goes first.

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
3. **Context per agent** — multi-series **line chart** over time, one series per
   agent, y = `context_tokens`, x = time. The metrics history store has **no**
   context column, so the series is **accumulated client-side**: a ring buffer in
   `Dashboard` (or a small `lib/contextHistory.ts` store) samples each live
   `Session.context_tokens` from the existing `/sessions` + SSE feed and appends a
   timestamped point. Survives tab switches (kept in app state above the tab); a
   full page reload starts the window fresh (documented limitation — a true
   persisted history is the §6 daemon follow-up). `context_state`
   (ok/warning/critical) colors the latest point / legend so pressure is visible.
4. **Number of agents** — single-series area/line, y = `system.agent_count`,
   x = time. Source: `getMetricsHistory()` → `sample.system.agent_count`.
5. **Tokens saved** — single-series bar/line of daily saved tokens. Source:
   `GET /savings?bucket=day` → `Summary.Buckets[].SavedTokens`, plus a headline
   number (`Summary.SavedTokens`, `SavedDollars`). Gated: if savings is disabled
   the daemon returns **403** — the card shows a friendly "enable `savings: true`
   in the config" message instead of an empty chart.

Each per-agent multi-series chart gets a stable color per agent id and a compact
legend. Empty/auth/disabled states render inline (no blank canvases).

### 4.5 Context & Messages → header button (not a tab)

*Context & Messages* is a read-only inspector and low-traffic, so it no longer
earns a top-level tab. It is **removed from the tab bar** and instead opened from
a **small icon button on the right side of the header** (`AttentionBar.tsx`),
next to the theme / help / notify controls — e.g. `🗒`.

- Clicking it opens `ContextMessagesTab`'s content as a **dismissible overlay**
  (right-side drawer or centered modal), mirroring the existing `ShortcutsHelp`
  overlay pattern. **Esc** closes it (wire into the existing keyboard layer in
  `Dashboard.tsx`).
- No route is consumed; it is overlay state (`showContext`) in `Dashboard`, not a
  navigable URL. (If we later want deep-linkable context, it can graduate back to
  a route — but the request is explicitly "place the button in the header".)
- `ContextMessagesTab.tsx` is reused as-is for the overlay body (optionally
  renamed `ContextMessagesPanel` for clarity).

---

## 5. Implementation plan

### 5.1 Routing layer (the core change)

The app is an Astro **static** SPA mounted once (`src/pages/index.astro` →
`<Dashboard client:only="react" />`). We introduce client-side routing without
adding a router dependency:

- **New `web/src/lib/router.ts`** — a tiny hash-free History-API helper:
  - `parseRoute(pathname): Route` → maps
    `/cockpit|/others|/pipelines|/metrics|/archive`→ fixed tab,
    `/agent/<id>`→`{kind:'agent', id}`, anything else → cockpit (default).
    (`/context` is no longer a route — it's a header-button overlay, §4.5.)
  - **`/` redirects to `/cockpit`** (single canonical URL): on load, if the path
    is `/` the app does `history.replaceState` to `/cockpit` before first render
    (a `replace`, not a `push`, so Back doesn't bounce). `/cockpit` is the home.
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
  ordered route list instead of dispatching reducer actions. Also owns the
  client-side context-history ring buffer feeding the Metrics tab (§4.4 item 3).
- **`TabBar.tsx`** — render `<a href>` anchors (with `onClick`→`navigate`,
  `preventDefault`) so tabs are real links (middle-click / open-in-new-tab work)
  and highlight via `route` match. Rename the `Overview` button → `Others`; add
  `Metrics`.
- **`lib/tabs.ts`** — refactor: `FIXED_TABS` becomes the route list
  `['cockpit','others','pipelines','metrics','archive']` (no `context`). Keep
  `orderedTabs` / index / nav helpers but key them off routes. Pinned-agent
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
- **Edit** `CockpitTab.tsx` — add the Fleet header (FleetStats) only.
- **Rename** `OverviewTab.tsx` → `OthersTab.tsx` — drop the Fleet, Quick spawn,
  and All-agents grid sections; keep *Needs you* (`AttentionQueue`), *File
  conflicts* (`ConflictsPanel`), and *Recent activity* (`ActivityFeed`). This is
  the catch-all tab going forward.
- **Edit** `Dashboard.tsx` — routing, switch, rewire Overview→Others + Metrics,
  own the context-history buffer, and add `showContext` overlay state (opened by
  the new header button, closed by Esc).
- **Edit** `TabBar.tsx` — links; rename Overview→Others; add Metrics; **remove
  the Context & Messages tab**.
- **Edit** `AttentionBar.tsx` — add a small right-side `🗒` button that toggles
  the Context & Messages overlay. Keep `+ New agent` as the single spawn path.
- **Reuse** `ContextMessagesTab.tsx` as the overlay body (optionally rename
  `ContextMessagesPanel`).
- **Delete** `QuickSpawn.tsx` (and its test/usages), `QuickAddButton.tsx` if
  unused after this. Move `ResourcesPanel.tsx` usage to Metrics (keep the
  component, or fold into MetricsTab).

### 5.3 Data sources (all already exist)

| Graph                 | Source (web client)                              | Backing route |
|-----------------------|--------------------------------------------------|---------------|
| A. CPU per agent      | `getMetricsHistory()` → `agents[].cpu_percent`   | `GET /metrics/history` |
| B. Memory per agent   | `getMetricsHistory()` → `agents[].rss_bytes`     | `GET /metrics/history` |
| C. Context per agent  | client-accumulated series from live `Session.context_tokens` / `context_state` (ring buffer in `Dashboard`) | `GET /sessions` + SSE (already in `Dashboard`) |
| D. Number of agents   | `getMetricsHistory()` → `system.agent_count`     | `GET /metrics/history` |
| E. Tokens saved       | **new** `getSavings()` client → `Buckets[]`      | `GET /savings?bucket=day` (exists) |

Only **one new client function** is needed: `getSavings(sinceISO?, bucket?)` in
`web/src/lib/api.ts` returning a `Summary` type mirrored in
`web/src/lib/savings.ts` (new, mirrors `internal/savings/savings.go` `Summary`).
Handle the 403 (disabled) path explicitly.

### 5.4 Tests

- Update `web/src/lib/tabs.test.ts` for the new route list
  (`cockpit,others,pipelines,metrics,archive` — no `context`).
- New `web/src/lib/router.test.ts` — `parseRoute`/`routeToPath` round-trips,
  `/`→`/cockpit` redirect, default-to-cockpit fallback, agent routes.
- New `web/src/lib/metricsSeries.test.ts` — series transforms, empty input.
- Adjust any test that imports the renamed `OverviewTab`→`OthersTab` or the
  deleted `QuickSpawn`.
- `npm run test` (vitest) green; `npm run build` (astro) green.

---

## 6. Daemon / server impact

**None required.** `internal/daemon/static.go` already serves `index.html` for
any non-API, non-file GET (SPA fallback), so `/cockpit`, `/metrics`,
`/agent/<id>` deep-links and refreshes resolve to the app. The `/savings`,
`/metrics`, `/metrics/history` routes already exist and are auth-gated like the
rest. No new endpoints, no route changes.

(One optional future follow-up, explicitly **out of scope**: add a context-token
column to the metrics recorder so *Context per agent* can be a **persisted**
historical series that survives reloads, instead of the client-accumulated
in-session series we build now. Tracked, not done here.)

## 7. Risks / edge cases

- **Per-agent series churn:** agents come and go; CPU/memory line charts must
  handle series appearing/disappearing across the history window without
  crashing uPlot (rebuild series defs when the agent set changes).
- **localStorage migration:** existing users have `warden.tabs` with
  `active:'overview'`. The new loader ignores stored `active` (URL wins) and
  only reads `pinned`; an old blob must not break parsing. (`'overview'` no
  longer exists as an active id — it's now `'others'`.)
- **Pinned-tab default route:** closing the last agent tab or landing on a stale
  `/agent/<deadid>` must fall back to `/cockpit` (mirror today's prune→overview
  behavior, retargeted to cockpit — the new default).
- **`/` redirect loop:** the `replaceState('/cockpit')` must run once on initial
  load only and never re-fire on subsequent `/cockpit` navigations.
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

## 9. Decisions (resolved with maintainer, 2026-06-26)

**D1 — Attention queue & conflicts placement → rename Overview to "Others".**
The Overview tab is **kept and renamed to "Others"** (`/others`). *Needs you*
(attention queue) and *File conflicts* **stay there**. Others is the designated
**catch-all** tab — the goto place for any new/not-yet-homed widget until it
finds a proper home.

**D2 — Recent activity feed → Others tab.** `ActivityFeed` **moves to (stays in)
the Others tab**, alongside *Needs you* and *Conflicts*.

**D3 — Context-per-agent graph → richer time series.** Use the **client-side
accumulated time-series line chart** (ring buffer over live `context_tokens`),
not a bar snapshot. Resets on full reload; a persisted series is the §6 daemon
follow-up.

**D4 — `/` canonical path → redirect.** `/` **redirects to `/cockpit`**
(`replaceState`) so there is a single canonical home URL.

**D5 — Context & Messages → header button, not a tab.** It's a low-traffic
read-only inspector, so it's **removed from the tab bar** and opened from a small
`🗒` button on the **right side of the header**, shown as a dismissible overlay
(Esc to close). No route consumed. See §4.5.
