# Monitoring layer separation — spec freeze

- **Status:** Locked (Phase 0 — spec freeze; docs only, no production code)
- **Repo:** warden. Baseline: `autopilot/monitoring-layer-separation` integration branch.
- **Scope:** Split `poller.tick()` into per-concern monitoring loops — a
  dedicated `TerminalWatcher` for terminal liveness, and a dedicated
  `PipelineWatcher` for pipeline health — so each subsystem runs at its own
  cadence and can own its own failure-recovery logic. This document is
  normative: the decisions in §2 are **locked** and are not to be relitigated
  during implementation.

---

## 0. Why this exists

Today `poller.tick()` handles two fundamentally different concerns inside a
single loop. Agent sessions (~200 lines: classify, rate-limit, context guard,
summaries, auto-approve) share the loop with terminal sessions (~15 lines:
liveness + pane capture) via an `if s.IsTerminal() { … continue }` branch.
Pipelines have no monitoring at all.

Three concrete bugs follow from this structure:

**Terminals poll at agent cadence.** Terminals only need liveness checks (is the
pane alive? did the exit-code change?). Running them through the full 30-second
agent loop — which loads rate-limit state, evaluates context pressure, processes
approval queues — is wasteful and makes it harder to tighten or loosen terminal
polling independently.

**Terminal auto-restore is broken.** `Restarter.onTransitionAt` fires only on
`store.StatusErrored`. Terminal sessions die to `store.StatusOrphaned` (the pane
disappears; no exit-code path fires). They are never restarted.

**Pipelines stall silently.** When a job agent transitions to `needs_attention`
nobody calls `Executor.Reconcile`. A running pipeline can sit idle indefinitely
because no background goroutine detects the stall or nudges the executor.

---

## 1. Phase breakdown

| Phase | PR target | Description |
|---|---|---|
| **0** | `autopilot/monitoring-layer-separation` | Spec freeze (this document). Docs only. |
| **1** | Phase 0 | `TerminalWatcher` struct + unit tests. No wiring. |
| **2** | Phase 1 | Wire `TerminalWatcher` into `server.go`; strip terminal branch from `poller.tick()`. |
| **3** | Phase 2 | Terminal auto-restore: extend `Restarter.onTransitionAt` for orphaned terminals. |
| **4** | Phase 3 | `PipelineWatcher` goroutine: startup reconcile, stuck-job watchdog, stall detection, orphaned-job cross-check. |

Phases are strictly sequential. Each phase opens a PR against the previous
phase's integration branch (not main) to keep diffs reviewable.

---

## 2. Locked decisions

### D1 — `TerminalWatcher` struct

New file: `internal/poller/terminal_watcher.go`.

```go
type TerminalDeps interface {
    List(ctx context.Context) ([]store.Session, error)
    SessionAlive(ctx context.Context, id string) (bool, error)
    CapturePane(ctx context.Context, id string) (string, error)
    UpdateStatusIf(ctx context.Context, id string, from, to store.Status) error
    UpdatePane(ctx context.Context, id string, pane string) error
    ExitCode(ctx context.Context, id string) (int, bool, error)
    ClearExit(ctx context.Context, id string) error
    Restore(ctx context.Context, id string) error
}

type TerminalWatcher struct {
    deps         TerminalDeps
    OnChange     func()
    OnTransition func(sess store.Session, from, to store.Status)
}

func (w *TerminalWatcher) Run(ctx context.Context, interval time.Duration)
```

`TerminalDeps` is a **subset** of `Poller.Deps` — no new daemon capabilities
are required. The interface boundary keeps the terminal watcher independently
testable without constructing a full poller.

`Run` ticks at `interval` (fed from config; see D5). On each tick it lists all
sessions where `s.IsTerminal()` is true and performs the same liveness + pane
capture logic that currently lives inside `poller.tick()`.

`OnChange` is called whenever any terminal session's state changes (for TUI
refresh). `OnTransition` is called whenever a session's `Status` changes (used
by `Restarter` — see D3).

### D2 — Strip terminal branch from `poller.tick()`

**Timing:** Phase 2 only, after `TerminalWatcher` is wired and tested.

```go
// top of poller.tick(), inside the session range loop
if s.IsTerminal() {
    continue
}
```

The existing `if s.IsTerminal() { … }` block (liveness check, pane capture,
exit-code handling) is deleted entirely. Terminal sessions must no longer appear
inside `poller.tick()` at all.

The guard is a two-line addition. The deletion removes ~15 lines. Both land in
the same Phase 2 commit so there is no window where terminals are monitored
twice.

### D3 — Terminal auto-restore

**Location:** `internal/poller/restarter.go` (or wherever `onTransitionAt` lives
today).

Extend `Restarter.onTransitionAt` to handle the orphaned-terminal case:

```go
// existing agent path — unchanged
if to == store.StatusErrored && sess.AutoRestart {
    life.Restore(ctx, sess.ID)
}

// new terminal path
if to == store.StatusOrphaned && sess.IsTerminal() && sess.AutoRestart {
    life.Restore(ctx, sess.ID)
}
```

`life.Restore` is the same call used for agent restarts. No new lifecycle method
is introduced. The existing `StatusErrored` path for agents is untouched.

This fix lands in Phase 3, after `TerminalWatcher` is wired, because
`onTransitionAt` will be driven by `TerminalWatcher.OnTransition` rather than
by `poller.tick()`'s inline transition detection.

### D4 — `PipelineWatcher` goroutine

New file: `internal/daemon/pipeline_watcher.go`.

**Launch site:** `server.go`, only when `s.exec != nil` (i.e. the pipeline
executor is present). Launched as a goroutine during daemon startup, cancelled
via the daemon's root context.

**Tick interval:** 60 seconds. Not configurable in Phase 4 (kept simple; can be
promoted to config in a follow-up if needed).

**Responsibilities per tick:**

1. **Startup reconcile.** On the *first* tick only, call `Executor.Reconcile`
   for every pipeline in `running` state. This catches daemon restarts that
   interrupted a mid-flight pipeline.

2. **Stuck job watchdog.** For each pipeline job in `needs_attention` state:
   - If `time.Since(job.UpdatedAt) >= pipeline.stuck_retry_after` **AND**
     `job.AutoRetryCount == 0` → call `Executor.Retry(pid, jobID)`.
   - Otherwise → notify the operator (e.g. emit an event / log a warning).
   The `AutoRetryCount == 0` guard prevents infinite retry loops: a job that
   has already been auto-retried once must be resolved by a human.

3. **Stall detection.** For each pipeline in `running` state: if no job's
   `UpdatedAt` has changed in >= `pipeline.stall_after`, call
   `Executor.Reconcile(pid)` as a safety net. This covers cases where a job
   completes but the executor fails to advance the pipeline.

4. **Orphaned job cross-check.** For each pipeline job that has an `agent_ref`
   but whose session ID is no longer present in the session store: mark the job
   `failed` + call `Executor.Reconcile(pid)`. This handles the case where an
   agent was deleted out-of-band.

`PipelineWatcher` **never spawns jobs** — that is `Executor.Reconcile`'s
responsibility. It only detects abnormal states and nudges the executor.

### D5 — Config additions

Four new config keys. All are duration/bool with safe defaults so they do not
require user action on upgrade.

| Key | Type | Default | Phase |
|---|---|---|---|
| `terminal_poll_interval` | Duration | `15s` | 1 |
| `pipeline.stuck_retry_after` | Duration | `10m` | 4 |
| `pipeline.stall_after` | Duration | `20m` | 4 |
| `pipeline.auto_retry` | bool | `true` | 4 |

`terminal_poll_interval` replaces the implicit "use the agent poll interval"
behaviour for terminals. Setting it lower (e.g. `5s`) tightens liveness
detection without affecting agent polling overhead.

`pipeline.auto_retry` is a master switch: when `false`, the stuck-job watchdog
only notifies instead of retrying regardless of `AutoRetryCount`.

### D6 — Job model: `AutoRetryCount` field

`pipeline.Job` gains one new field:

```go
AutoRetryCount int `json:"auto_retry_count"`
```

`PipelineWatcher` increments this field when it auto-retries a stuck job.
Serialised to the pipeline's stored record. The field is additive — existing
records that lack the key deserialise to zero (no migration needed).

---

## 3. Non-changes (explicitly out of scope)

- No changes to `store.Session.Kind`, `store.Session.AutoRestart`, or any
  existing session store routes.
- No new API endpoints. No changes to `openapi.yaml` in any phase of this work.
  The spec-first API rule (edit openapi.yaml → `make generate`) applies if a
  future phase does need an endpoint, but none are anticipated.
- No changes to the TUI or CLI surface.
- `PipelineWatcher` tick interval (60s) is intentionally not configurable in
  Phase 4; leave that for a follow-up.
- The `TerminalDeps` interface is a subset of `Poller.Deps`. No new daemon
  capabilities are introduced in any phase.

---

## 4. Open questions (informational, not blocking)

These were flagged during design review but are explicitly deferred:

- **Backoff on orphaned-terminal restore.** Today `life.Restore` is called
  immediately. If the underlying cause (e.g. tmux server crash) persists, warden
  will spin-restore repeatedly. A cap or backoff may be desirable. Deferred to a
  follow-up after Phase 3 ships and real-world behaviour is observed.
- **PipelineWatcher notification channel.** The stuck-job "notify operator" path
  (D4 item 2) currently means "log a warning." A proper notification channel
  (TUI event, SSE push) is desirable but not required for Phase 4 correctness.
