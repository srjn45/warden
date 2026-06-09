# warden observability surface — design

**Date:** 2026-06-09
**Status:** Approved for planning
**Feature:** A resource-first observability surface for the warden daemon — a live `GET /metrics` snapshot, an always-on JSONL recorder, and in-app history read-back — exposing per-agent memory/CPU attribution, system memory pressure, and the daemon's own footprint. Consumed via `warden stats` (CLI) and a web Resources panel. Designed so daemon ops metrics can slot in later without rework.

## Motivation

The freeze investigation ([memory: agentctl-freeze-investigation]) concluded the Mac UI
freezes are most likely **memory-compressor thrash** driven by many long-lived **1M-context**
Claude agents — worst when several compact at once. The shipped mitigations are *behavioral*:
the pipeline-decomposition nudge (preventive) and self-rotate (reactive). Both bound an agent's
context. Neither gives us **visibility**: warden today is blind to its own footprint. It runs
`log.Printf` to `/tmp`, has no metrics, and cannot answer "which agent is eating the machine
right now?" or "what did the ramp into the last freeze look like?"

An external memory sampler LaunchAgent was installed to catch the next freeze, but it samples
the whole machine — it **cannot attribute memory to individual warden agents**, because it does
not know which tmux pane belongs to which agent. Warden does. This feature moves resource
sampling *in-process*, where the agent→tmux→PID mapping is already known, and records a trail
that survives the freeze so it can be analyzed afterward.

The freeze is intermittent and hits when the machine is already thrashing — you usually cannot
be watching a live view at the moment it happens. So a live snapshot alone is insufficient: the
recorder must be **always-on**, capturing the ramp *before* you know you need it.

## Goals

- **Per-agent attribution.** See RSS, CPU%, process count, and uptime for each agent, sorted so
  the hog is obvious.
- **System + daemon context.** Total/used/free/wired/compressed memory, swap used, memory-pressure
  level, total warden-attributed RSS, agent count; plus the daemon's own RSS, goroutine count, and
  open FDs.
- **Always-on recording.** A trail on disk that captures the lead-up to a freeze without manual
  arming.
- **Live + historical consumption.** A live JSON snapshot and an in-app read-back of recent
  history, surfaced via CLI (`warden stats`) and the web Overview tab. **No TUI surface** — the
  TUI is deliberately kept uncluttered.
- **Extensible.** The data model leaves room for daemon ops metrics (request latency, error rates,
  SSE/WS counts) as a clean future addition.

## Non-goals (v1)

- **Daemon ops metrics** (request latency / error rates / poller-tick timing / SSE-WS counts).
  The `SystemStats`/`DaemonStat` shapes leave room; building them is deferred.
- **TUI display.** Web-only, by user request (avoid TUI clutter).
- **Prometheus text-exposition format.** Considered and dropped; JSON only.
- **Alerting / thresholds / auto-action** (e.g. auto-rotate when RSS crosses a line). Out of scope;
  this feature only *observes*.
- **Cross-platform parity.** Sampling is macOS-first (`vm_stat`, `sysctl`, BSD `ps`); other
  platforms degrade gracefully rather than being a target.

## Architecture

A new package **`internal/metrics`**, mirroring the `pressure`/`digest` pattern: pure logic in the
core, exec only at the edge. The impure parts (`ps`, `tmux`, `vm_stat`, `sysctl`) are thin runner
calls; everything that can be *wrong* (process-tree aggregation, byte math, `etime` parsing) lives
in pure functions tested against captured fixtures — the same discipline as the approvals parser.

This is **additive**, not a rewrite. The daemon already has a `runPressureSampler` goroutine, a
`GET /pressure` endpoint, and a web `FleetStats` pressure indicator. `/metrics` is a superset: the
collector reads the *already-cached* pressure level from the daemon rather than re-running sysctl.

### Files

- **`types.go`** — serializable shapes (see Data model).
- **`parse.go`** — *pure* parsers and aggregation, the bulk of the testable logic:
  - `vm_stat` page counts → bytes (using the reported page size)
  - `sysctl vm.swapusage` → swap-used bytes; `sysctl hw.memsize` → total bytes
  - `ps -axo pid=,ppid=,rss=,%cpu=,etime=` → a process table
  - process-*tree* aggregation: sum RSS/CPU and count processes over all descendants of each tmux
    pane PID; `etime` → uptime seconds
- **`collect.go`** — `Collector`, with two narrow injected deps so it is unit-testable:
  - `Lister` — just `store.List(ctx) ([]*store.Session, error)`
  - `Runner` — the existing `lifecycle.Runner` (`Run(ctx, dir, name, args...) (string, error)`)
  - `Collector.Sample(ctx) (Sample, error)`: list agents → for each running one resolve
    `tmux list-panes -F '#{pane_pid}' -t <id>` → **one** `ps -ax` scan → aggregate per-agent and
    totals → read daemon self-stats (`runtime.NumGoroutine`, own RSS from the same `ps` table,
    best-effort FD count) → fold in the cached pressure level. Bounded cost: one `ps` per sample,
    not one per agent.
- **`recorder.go`** — `Recorder`: the always-on goroutine + history read-back (see Recorder).

### Why injected interfaces

The `Collector` never imports `daemon` or constructs a `Lifecycle`. The daemon wires concrete
implementations (`s.store`, the lifecycle runner) at construction. Tests inject a fake `Runner`
returning fixture strings and a fake `Lister` returning canned sessions, so attribution logic and
graceful degradation are tested with zero real processes.

## Data model (`types.go`)

```go
type Sample struct {
    TakenAt time.Time   `json:"taken_at"`
    System  SystemStats `json:"system"`
    Agents  []AgentStat `json:"agents"`
    Daemon  DaemonStat  `json:"daemon"`
}

type SystemStats struct {
    TotalBytes         uint64 `json:"total_bytes"`
    UsedBytes          uint64 `json:"used_bytes"`
    FreeBytes          uint64 `json:"free_bytes"`
    WiredBytes         uint64 `json:"wired_bytes"`
    CompressedBytes    uint64 `json:"compressed_bytes"`
    SwapUsedBytes      uint64 `json:"swap_used_bytes"`
    PressureLevel      string `json:"pressure_level"` // "normal" | "warn" | "critical"
    AgentCount         int    `json:"agent_count"`
    AttributedRSSBytes uint64 `json:"attributed_rss_bytes"` // sum over agents
}

type AgentStat struct {
    ID         string  `json:"id"`
    Status     string  `json:"status"`
    Paneable   bool    `json:"paneable"`    // false if tmux/ps lookup failed
    RSSBytes   uint64  `json:"rss_bytes"`
    CPUPercent float64 `json:"cpu_percent"`
    ProcCount  int     `json:"proc_count"`
    UptimeSec  int64   `json:"uptime_sec"`
}

type DaemonStat struct {
    RSSBytes   uint64 `json:"rss_bytes"`
    Goroutines int    `json:"goroutines"`
    OpenFDs    int    `json:"open_fds"` // best-effort; 0 if unavailable
}
```

## Integration surfaces

### Daemon (`internal/daemon`)

- `Server` gains a `*metrics.Collector` (built in `New(...)` from `s.store` + the lifecycle runner)
  and a `*metrics.Recorder`.
- Two routes in `router()`:
  - `GET /metrics` → `handleMetrics`: calls `collector.Sample(ctx)`, returns the live `Sample`.
  - `GET /metrics/history?since=<RFC3339>&limit=<n>` → `handleMetricsHistory`: reads recorded
    samples from disk, newest-first, bounded. Defaults: last ~2h, capped count (e.g. 1000) so a
    wide-open query cannot dump days.
- The recorder goroutine is started in `ListenAndServe` next to `runPressureSampler` and stopped
  via the same `runCtx` cancellation — no new shutdown plumbing.
- Gated by a new `WARDEN_METRICS` flag in `internal/config` (on by default; `0/off/false`
  disables), parsed exactly like `WARDEN_SPAWN_GATE`. When off, the routes still serve a live
  snapshot but the disk recorder does not run.

### CLI + client

- `internal/client`: `GetMetrics(ctx)` and `GetMetricsHistory(ctx, since, limit)` via the existing
  `c.do` GET pattern.
- `internal/cli`: one new command `warden stats`:
  - default — a compact human table: a system summary line + per-agent RSS/CPU/uptime rows, sorted
    by RSS descending (hog on top).
  - `--json` — the raw `Sample` (the `printJSON` pattern).
  - `--watch` — redraw every few seconds for live terminal triage.

### Web (`web/src`) — web-only

- `lib/api.ts`: `getMetrics()` + `getMetricsHistory()`; `lib/types.ts`: matching `MetricsSnapshot`
  types.
- New **`ResourcesPanel.tsx`**, added as a section in the existing **Overview** tab:
  - **Live block** — system memory/pressure/swap summary + a per-agent table (RSS bar, CPU%,
    uptime), polling `/metrics` every ~5s (the existing `FleetStats` `setInterval` pattern).
  - **History block** — a time-series chart of total attributed RSS + system pressure over the
    last ~2h from `/metrics/history`, so you can *see the ramp*.
- **Charting:** **uPlot** (~40KB, canvas-based, purpose-built for time-series). Chosen over
  recharts for bundle weight; the dataset is small (~480 points at 15s over 2h). A small pure
  helper shapes `Sample[]` → uPlot series so the shaping is unit-testable independent of render.

## Recorder & on-disk format

- Writes to `~/.warden/metrics/YYYY-MM-DD.jsonl` — one `Sample` per line, append-only, surviving
  restarts. The directory is created `0o700` (this data is the same sensitivity class as session
  files; the session-dir `0o755→0o700` permission fix is folded in here).
- **Cadence:** ~15s ticker.
- **Rotation/retention:** a new file per day by filename; on startup and once per day, prune
  day-files older than 7 days. History read-back globs the day-files, filters by `since`, and caps
  by `limit`.

## Error handling

Collection must **never** destabilize the daemon.

- Collection is entirely best-effort: a failed `tmux`/`ps`/`vm_stat`/`sysctl` call degrades that
  one field (agent → `Paneable:false`, RSS 0; system field → 0) and logs at most once per failure
  class. It never aborts the whole sample.
- A dead agent mid-scan simply drops out (its pane PID resolves to nothing).
- The recorder goroutine wraps each tick with panic recovery so a sampling bug cannot take down
  the daemon (matches the existing `recoverMiddleware` philosophy).
- `/metrics/history` on a missing/unreadable day-file returns what it can, not a 500.
- Bounded cost everywhere: one `ps -ax` per tick (not per-agent), a 7-day disk cap, and capped
  history queries.

## Testing

- **Pure parsers (`parse.go`)** against captured fixtures: real `vm_stat`, `sysctl vm.swapusage`,
  `sysctl hw.memsize`, and `ps -axo …` output strings → assert byte math, `etime`→seconds, and
  process-tree RSS/CPU aggregation. The densest test file — this is where the logic risk lives.
- **`Collector`** with a fake `Runner` + fake `Lister` → attribution maps correctly; degrades
  gracefully when a runner call errors (agent marked non-paneable, sample still returned).
- **`Recorder`** against a temp dir → write / daily-rotate / 7-day-prune + history read-back with
  `since`/`limit` bounds, using injected sample timestamps (no wall-clock in the pure path).
- **Daemon routes** via `httptest` (existing pattern): `/metrics` and `/metrics/history` happy
  paths + bounded-query behavior.
- **Web**: a unit test for the `Sample[]`→uPlot-series shaping helper.

## Future work

- Daemon ops metrics (request latency, error rates, poller-tick duration, SSE/WS connection
  counts) — the data model already reserves space.
- Threshold-based hints (e.g. surface a "consider rotating agent X" nudge when attributed RSS or
  pressure crosses a line) — ties into self-rotate / the shelved context gauge
  ([memory: agentctl-token-cost-shelved]).
- TUI display, if the web surface proves the value and a compact non-cluttering form is found.
- Prometheus exposition format, if external scraping is ever wanted.
