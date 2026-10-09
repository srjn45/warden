# Agent-store lock-contention contract (Cockpit incident)

Status: design record + executable baseline. No production behavior changes in
this task (only two nil-by-default diagnostic seams, §9).

Related: `2026-10-06-agent-store-integrity-contract.md` (ownership, complete-or-error
reads — this record keeps those guarantees and changes *how* they are paid for),
`2026-10-07-agent-store-integrity-validation-report.md`.

## 1. Incident

The Cockpit TUI polls the fleet every second (§2). Since the integrity work, every
`agentstore` read performs a **full verified collection scan while holding the
store's single `sync.Mutex`**, and every write takes the same mutex. Consequences,
all reproduced by the baseline (§3):

1. A reader excludes every writer and every other reader. N pollers = N serial scans.
2. A writer (poller status/pane/activity writes, hooks, lifecycle) excludes every reader,
   including `/healthz` (`Ping` takes the mutex).
3. "Point" reads (`Get`, `GetByNameOrID`) and every `Insert` scan the whole collection.
4. `ctx` is only checked on entry, so a deadline does not release a queued caller.
5. Caller-supplied `Update` callbacks run under the mutex.

Measured (§3.3, this checkout, in-process, tmpfs-class disk): scan ≈ 60 µs/agent;
with 4 concurrent Cockpit-style pollers a single status write waits p50 **249 ms /
p99 321 ms at 1000 agents** versus **0.14 ms idle** — a ~1800× write amplification
caused purely by lock queueing. The visible symptom is Cockpit rows freezing/flapping
and `/healthz` and spawn/hook calls timing out against the 10 s `bg()` client budget
(`internal/tui/cmds.go`) while no individual operation is "slow".

## 2. Trace: every AgentStore reader, writer, and Cockpit request path

`agentstore.Store` (`internal/agentstore/store.go`) implements `AgentStore`
(`types.go:339`). One `Store` per daemon, held under the ownership flock; the mutex
`Store.mu` is the *only* in-process synchronization. Counts below are non-test call
sites outside `internal/{planstore,pipeline,tui,cli,client}` (regenerate with the
grep in §10).

### 2.1 Readers (all take `mu`; all but `Ping` scan the whole collection)

| Method | Cost today | Call sites (files) | Cockpit-hot? |
|---|---|---|---|
| `List` | 1 full verified scan, sorted | 44 sites: `sse.go`, `strict_core.go` (ListSessions, ListApprovals), `tree_routes.go`, `store_health.go`, `pressure_routes.go` (spawn gate), `lifecycle_routes.go`, `strict_lifecycle.go` ×5, `strict_projects.go`, poller (`poller_deps.go`), `ratelimit.go`, `backend_recovery.go`, `executor.go`, `autopilot_*`, `collab/monitor.go` ×3, `branchtrack`, `land_loop.go`, `usage_*`, `worktree_prune.go`, `tombstone_reap.go`, `metrics_routes.go`, `peer_context.go`, `snapshot`, `router/resolver.go`, … | **yes** (sessions, SSE, approvals, tree, health, poller every tick) |
| `Get` | **1 full scan** + linear id match (not a point read) | 60 sites: autopilot runtime ×11, `strict_core.go`, `backend_recovery.go`, `pipeline_watcher.go`, `land_loop.go`, `plan_executor_status.go` (per plan row), `ratelimit.go`, `usage_*`, `recover.go`, `scheduler.go`, … | yes (GetSession, GetOutput via `resolveSession`, plan list rows) |
| `GetByNameOrID` | 1 full scan; id-fallback then goes through `s.get` | 7: `strict_lifecycle.go` ×2, `lifecycle_adapter.go`, `ownership_guard.go`, `land_routes.go`, `strict_models.go`, `strict_obs.go` | yes (every `{id}` route resolving a name) |
| `ListClosed` / `ListClosedDegraded` | 1 full scan of `closed`, tolerant decode | 5: `strict_lifecycle.go` ×3, `strict_collab.go`, `worktree_prune.go` | history/restore |
| `Ping` | takes `mu`, no scan | `api.go:handleHealthz` | **`/healthz`** |
| (internal) `get(id)` | engine point read + verify; used by writers only | `Update`, `Archive`, `UpdateStatusIf`, `FinalizeExit`, `GetByNameOrID` fallback | — |

### 2.2 Writers (all take `mu`; run the engine write inside it)

| Method | Extra work under `mu` | Call sites |
|---|---|---|
| `Insert`/`Create`/`Spawn` | full scan for name uniqueness (names are mandatory ⇒ **always**) | 12: `strict_lifecycle.go` ×3, `autopilot_*`, `executor.go`, `fix_runtime.go`, `brainconsult_spawner.go`, `plan_agent_adapters.go`, `schedule_routes.go` |
| `Update` (+ all `Update*`, `Set*`, `AppendEvent*`, `StampCompact`, `Init`, `Terminate`, `Recover`, `ClearWorktree`) | point `get`; **caller callback `fn` runs under `mu`** | 30 direct `Update`; wrappers in `poller_deps.go` (`UpdatePane`, `UpdateActivity`, `UpdateContext`, `StampCompact`, `SetSessionID` — **per agent per poll tick**), `backend_recovery.go` ×10, `ratelimit.go` ×16, `strict_lifecycle.go`, `child_agents.go` ×4, … |
| `UpdateStatus`, `UpdateStatusIf` (CAS), `FinalizeExit` | point `get` | `poller_deps.go`, `ratelimit.go` ×5, `autorestart.go`, `backend_recovery.go`, `usage_sync.go`, `land_loop.go`, `strict_lifecycle.go` ×4 |
| `Archive` | `get` + write `closed` + delete `agents` (two engine ops, one lock) | 8: plan finalize/restart, autopilot, `strict_lifecycle.go`, `strict_pipeline.go` |
| `Delete` | one engine delete | 9: autopilot, `executor.go` ×3, `strict_lifecycle.go` |

Out-of-band openers (not contention sources, constrained by ownership):
`cli/daemon.go` (`New`), `cli/doctor.go`, `agentstore/repair.go` (offline), `ReconcileProjectMembership`
(boot + doctor; uses `List`/`Update` through the same `Store`).

### 2.3 Cockpit request paths

Cockpit = the TUI (`internal/tui`) plus every client that renders the same node API
(web cockpit, Android app via SSE/relay). All of them funnel into the same `Store.mu`.

| Path | Trigger | Store reads | Store writes |
|---|---|---|---|
| TUI `tickMsg` (`control_pane.go:917`) → `listCmd` → `GET /api/v1/sessions?all=true` | **every 1 s**, 10 s client budget (`bg()`) | `List` | — |
| TUI tick → `approvalsCmd` → `GET /approvals` | 1 s | `List` (+ tmux/pane parse per waiting row) | — |
| TUI tick → `healthCmd` → `/healthz` | 1 s | **`Ping`** | — |
| TUI tick → `plansCmd` → `ListPlans` → `planExecutorStatus` | 1 s | `Get` **per plan row** (N plans ⇒ N scans) | — |
| TUI tick → `autopilotCmd` → `GET /autopilot` | 1 s | none directly (controller memory) | — |
| TUI tick → pipelines / projects / project-groups / pressure | 1 s | none (own stores; pressure cached by sampler) | — |
| TUI inspector (`contextCmd`, `messagesCmd`, `Output`) | 1 s while open | `GetByNameOrID`/`Get` via `resolveSession` | — |
| `GET /events` SSE (`sse.go`) — web cockpit, app | initial + **every `s.notify()`** (hub coalesces) + 25 s heartbeat | `List` per fire; also `treeInputsFor` | — |
| `GET /tree` | on demand | `List` | — |
| `GET /store/health` | on demand / monitor | `List` (it *is* a full scan) | — |
| Poller tick (`poller.tick`, every configured interval) | background | `List` then per agent: `UpdateStatusIf`, `UpdatePane`, `UpdateActivity`, `UpdateContext`, `FinalizeExit`, `AppendEvent` | **N writes per tick**, each queuing behind readers; ends with `notify()` which wakes every SSE client → **another `List` per client** |
| Hooks `POST /events` | per agent hook | — | `AppendEventStatus` |
| TUI actions (spawn, kill, rename, auto-approve, …) | user | `GetByNameOrID`, `List` (name check), `Get` | `Insert`, `Update*`, `Archive`, `Delete` |
| Background loops | timers | `List`/`Get` | `ratelimit`, `backend_recovery`, `usage_sync`, `land_loop`, `autopilot_runtime`, `worktree_prune`, `tombstone_reap`, `collab/monitor` |

**Amplification loop (the incident shape):** poller writes N rows → `notify()` →
each SSE client and each TUI poll issues a scan → scans queue ahead of the *next*
tick's writes → tick duration grows with `clients × agents × 60 µs` → status flaps
and the 10 s client budget is consumed by lock wait. Nothing in the chain sheds
load; every participant is individually correct.

## 3. Baseline: reproduced contention (executable)

Both files pass today and **characterize current behavior**. They are the
before-picture; a fix task must flip the matching assertion.

### 3.1 Seams (§9) and tests

| Test | Proves |
|---|---|
| `agentstore.TestBaselineSlowScanBlocksWriters` | a parked scan blocks `Update`, `UpdateStatusIf`, `AppendEvent`, `Insert`, `Delete`, `Ping` |
| `agentstore.TestBaselineSlowWriteBlocksReaders` | a parked write blocks `List`, `Get`, `GetByNameOrID`, `ListClosed`, `Ping` |
| `agentstore.TestBaselineReadersSerialize` | two concurrent 120 ms scans take ≥ 240 ms |
| `agentstore.TestBaselineScanCountPerOperation` | scans per call: Get/List/GetByNameOrID/ListClosed*/Insert = 1; Update/CAS/Archive/Delete = 0 |
| `agentstore.TestBaselineContextIgnoredWhileQueued` | ctx with 50 ms deadline stays queued >250 ms and then *succeeds* |
| `agentstore.TestBaselineUpdateFnRunsUnderLock` | a slow `Update` callback blocks readers |
| `agentstore.TestBaselineDegradedStoreFailsFastWithoutStale` | preflight-degraded `List` fails fast; no stale view exists |
| `daemon.TestBaselineCockpitPollStallsBehindSlowStoreWrite` | with one parked store write, `GET /sessions`, `/tree`, `/store/health`, `/approvals`, `/sessions/{id}`, `/healthz` all fail to answer inside 400 ms; releasing the write releases them with 200 |
| `agentstore.TestBaselineLatencyProfile` (opt-in) | produces §3.3 |

Run: `go test ./internal/agentstore ./internal/daemon -run 'Baseline' -count=1`.
Profile: `WARDEN_BASELINE_PROFILE=1 go test ./internal/agentstore -run LatencyProfile -v`.

### 3.2 Reading the daemon-level result

The daemon test uses the real `Store`, the real router and a parked write via the
seam — it is not a mock of the lock. Passing means "stalls today". After the fix
the same test is rewritten to assert the opposite (answers < 100 ms from snapshot,
`X-Warden-Store-State: ok`) and the baseline name is deleted.

### 3.3 Measured profile (this checkout, 2026-10-09)

| agents | scan | write, idle | write under 4 pollers p50 | p99 |
|---|---|---|---|---|
| 50 | 2.2 ms | 0.11 ms | 33 ms | 61 ms |
| 200 | 10 ms | 0.13 ms | 67 ms | 99 ms |
| 1000 | 60 ms | 0.14 ms | 249 ms | 321 ms |

Scan cost is linear (~60 µs/agent). Seeding 3000 agents is itself quadratic
(every `Insert` rescans) and was dropped from the profile — that is finding 3 above
observed first-hand.

## 4. Contract

### 4.1 Snapshot consistency

Definitions. A **snapshot** is an immutable, versioned, in-memory image of both
collections (`agents`, `closed`) published as one value behind an atomic pointer:
`{version uint64, builtAt, agents []*Agent (sorted), byID, byName, closed …, verified bool}`.

Invariants (each has a gate test in `contention_contract_test.go`):

- **I-1 Reads never block writes, I-2 writes never block reads.** Reads load the pointer;
  no mutex shared with the write path. (`TestContractContentionScanDoesNotBlockWriters`,
  `…WriteDoesNotBlockReaders`)
- **I-3 Point reads are O(1)** (`byID`/`byName`); `Insert` name uniqueness uses `byName`
  under the write lock, not a scan. (`…GetIsPointRead`)
- **I-4 Writers are serialized; callbacks do not extend exclusion unboundedly.**
  `Update` computes on a private copy; the callback must not call the store (documented;
  `warden_agentstore_update_fn_seconds` makes violations visible). A callback never runs
  while readers wait, because readers do not wait.
- **I-7 Atomic publication.** A mutation becomes visible only after the engine commit
  succeeded, by swapping in a *new* snapshot value. `Archive` moves a row between
  collections in **one** swap: no reader ever sees the agent in both or neither.
  A failed engine write leaves the published snapshot untouched.
- **I-8 Read-your-writes.** The snapshot containing a write is published before the
  write method returns, so any read that begins after the write returned observes it.
  Across callers the guarantee is *monotonic*: `version` never decreases for a given
  process; two reads in order never go backwards.
- **I-9 Isolation of returned data.** Returned `*Agent` values are copies (deep for
  slices/maps/pointers); mutating a result never mutates the snapshot.
- **I-10 Whole-fleet reads are internally consistent.** Every list/tree/SSE frame is
  derived from one snapshot version; handlers that need several reads (tree, approvals)
  take one snapshot and use it for all.
- **Source of truth.** The engine remains authoritative. The snapshot is built once at
  `New` by the existing verified scan (so complete-or-error from the integrity
  contract still holds at the boundary) and then maintained write-through. It is a
  cache of the engine, never an independent store.
- **Complete-or-error is preserved, relocated.** A published snapshot is `verified`
  (count == scan, identity, no duplicates) at build; the per-read scan verification
  moves to build time and to the auditor (§4.3). A read can never return a short list
  that was not already a verified-complete snapshot.

### 4.2 Stale / degraded semantics

States of the store, exported as `warden_agentstore_state` and per-response metadata:

| State | Meaning | Reads | Writes |
|---|---|---|---|
| `ok` | latest snapshot verified; no open findings | 200, current | accepted |
| `suspect` | a cheap on-path check or write-through failure raised doubt; audit queued | 200 from the last **verified** snapshot, labelled `stale=false,suspect=true` | accepted unless the failing op was a write (that op errors) |
| `degraded` | auditor (or boot preflight) confirmed an integrity failure | 200 from last-known-good snapshot **labelled stale** while `age ≤ stale_max_age` (default generous: 10 m, configurable; per frictionless-safeguards, never tightened for normal work); after that 503 with `UnhealthyError` | **refused** with `UnhealthyError` (today's behavior; never write on top of a known-damaged store) |
| `unavailable` | no verified snapshot ever existed (boot preflight failed, nothing to serve) | 503 | refused |

Rules:

1. **Never a partial list.** Stale means "an older complete verified view", never "some rows".
2. **Never silent.** Every Cockpit response carries `X-Warden-Store-State` (`ok|suspect|stale|unavailable`),
   `X-Warden-Snapshot-Version`, `X-Warden-Snapshot-Age-Ms`. The SSE sessions frame gains an
   additive `meta` object with the same fields; existing clients that ignore it keep
   current behavior. TUI shows a one-line banner on `stale`/`unavailable` and keeps its
   own last-known-good (already implemented client-side).
3. **Latching.** `degraded` only clears by (a) a clean audit following `warden repair`
   (offline, ownership-guarded — unchanged), or (b) daemon restart passing preflight.
   A transient read error does not latch (it yields `suspect` and a re-audit).
4. **No stale writes.** Stale data is read-only. A stale response must never be used as the
   basis for a write inside the daemon: internal callers that decide-then-write
   (`UpdateStatusIf`, name uniqueness, lifecycle) use the write-path state, not a stale
   snapshot; the API exposes the snapshot version so CAS-style callers can detect it.
5. **`/healthz` is liveness only.** It must not depend on store state: store condition is
   reported via `/store/health` and `meta`. (Today `Ping` can hang `/healthz`; target:
   `Ping` is lock-free and returns `closed`/`ok` only.)
6. **`/store/health` stops scanning.** It reports the auditor's last result plus live state
   and optionally `?probe=1` to force an audit (admin action, bounded, §4.3).

### 4.3 Integrity-audit authority

Today every read re-derives integrity inline (`scanVerified`), so *any reader* can
declare the store unhealthy and the cost is paid on the request path. Target authority:

| Actor | May do | May not |
|---|---|---|
| **Boot preflight** (`New`, existing `VerifyAgentStore`) | decide initial `ok` vs `degraded`; choose scratch-copy open | — |
| **Request-path reads** | O(1) checks only (record key==id on point reads, snapshot pointer non-nil). On failure: raise `suspect`, enqueue an audit, return the last verified snapshot | latch `degraded`; scan the collection; take the write lock |
| **Write path** | verify its own engine write (key/id echo, count delta == expected); on mismatch return the error, raise `suspect` | latch `degraded` on its own |
| **Auditor** (single goroutine, `agentstore.Auditor`) | sole authority to move `suspect→ok` and `ok|suspect→degraded`; runs `engine.VerifyDir` on the on-disk tree and a snapshot-vs-engine diff; records findings | hold the write lock for the scan; run concurrently with itself; mutate data (repair stays offline CLI) |
| **Repair** (`warden repair agents`, offline) | the only mutator of a damaged store | run while the daemon owns it (unchanged) |

Cadence and bounds: audit at boot (after preflight), then every `audit_interval`
(default 10 m, jittered), plus on-demand when `suspect` is raised (debounced 5 s) and
via admin `?probe=1`. One audit at a time; duration cap generous (default 5 m) after
which it records `result=timeout` and does **not** degrade the store. The auditor reads
the engine through a path that does not take the write mutex (the engine is safe for
concurrent readers; if a future engine is not, the auditor works on a point-in-time
copy as boot preflight already does with `copyTree`). Divergence between snapshot and
engine is itself a finding (`class=divergence`) and triggers a snapshot rebuild from the
engine before any degrade decision.

### 4.4 Cancellation guarantees

| Situation | Guarantee |
|---|---|
| Read (lock-free) | returns `ctx.Err()` only if ctx was already done on entry; never blocks, so nothing to cancel |
| Writer queued for the write lock | acquisition is ctx-aware (channel semaphore). On cancel/deadline the call returns `ctx.Err()` within **50 ms** of the deadline, **before any engine mutation** — the state is exactly as if the call never happened |
| Writer past the commit point | the engine write **completes** and the call returns its true result (nil or error), never `ctx.Err()` after a successful commit. Cancellation after commit cannot produce a half-applied or "failed-but-applied" write. `Archive` (two engine ops) is a single commit unit: both ops run or neither is started |
| `Update` callback | receives a ctx that is cancelled with the caller's; a callback that returns after cancel aborts the write (pre-commit) |
| Snapshot rebuild / audit | own bounded ctx derived from the daemon root context, **never** from a request ctx (a client disconnect must not abort a rebuild nor leave a half-built snapshot); `Close` cancels and waits |
| Shutdown | `Close` stops the auditor, drains the writer queue with `ErrClosed`, then releases the ownership lock (existing order preserved) |
| HTTP | strict handlers pass `r.Context()`; an abandoned request frees its queue slot; `warden_agentstore_ctx_abandoned_total{phase}` counts it |

### 4.5 SLOs (proposed, measured at 1000 agents; reviewed after the fix lands)

Budgets are intentionally generous backstops, not pacing devices (see
frictionless-safeguards): they define when the dashboard goes red, and no code
path throttles normal work to meet them.

| SLI | SLO (28-day, 99% unless noted) | Today (baseline) |
|---|---|---|
| Snapshot read (`List`/`Get`/`GetByNameOrID`/`Ping`) latency | p99 ≤ 5 ms, independent of fleet writers | 60 ms+ (scan), unbounded under writer |
| Writer lock wait (`mutex_wait`) | p99 ≤ 25 ms regardless of reader count | 321 ms @1000 agents / 4 pollers |
| Write op end-to-end (`Update`, CAS) | p99 ≤ 50 ms | 321 ms |
| `GET /sessions` (Cockpit poll) | p99 ≤ 100 ms; ≥ 99.9% non-5xx (stale counts as success) | stalls to client timeout |
| `/healthz` | p99 ≤ 20 ms, independent of store | blocks behind any scan/write |
| Snapshot freshness (healthy) | `snapshot_age` ≤ time since last write + 1 s; every committed write visible to next read (I-8) — 100% | n/a (always fresh, always expensive) |
| Stale-serving bound | stale responses only in `degraded`, age ≤ `stale_max_age` | n/a |
| Cancellation | queued writer returns ≤ 50 ms after deadline: 99.9% | never (ctx ignored) |
| Audit | completes within cap: 99%; last success ≤ 2× interval: always, else alert | none |
| Queue depth | `lock_waiters` p99 ≤ 4 | grows with clients |

## 5. Metrics contract

Names are the contract; the exposition mechanism is chosen by the implementing task
(the repo has no `/metrics` today — `internal/metrics` is JSON resource sampling).
Whatever the carrier (Prometheus text at `/api/v1/metrics/prom`, or a `store` section
in the existing JSON sample), the names, types, labels and units below are fixed.
Label values are low-cardinality enumerations; **no agent id/name labels**.

Store (`internal/agentstore`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `warden_agentstore_op_total` | counter | `op`, `result` = `ok|not_found|conflict|error|canceled|unhealthy` | every public method |
| `warden_agentstore_op_duration_seconds` | histogram | `op`, `class` = `read|write` | end-to-end including queueing |
| `warden_agentstore_lock_wait_seconds` | histogram | `op`, `class` | time blocked on `mu` (today) / write semaphore (target) |
| `warden_agentstore_lock_hold_seconds` | histogram | `op` | time `mu` held |
| `warden_agentstore_lock_waiters` | gauge | `class` | current queue depth |
| `warden_agentstore_scan_duration_seconds` | histogram | `collection`, `phase` = `count|scan|verify|decode` | scan cost split |
| `warden_agentstore_scans_total` | counter | `collection`, `caller` = `read|snapshot_build|audit|insert_name` | **target value for `read` and `insert_name` is 0** |
| `warden_agentstore_records` | gauge | `collection` | rows (agents / closed) |
| `warden_agentstore_update_fn_seconds` | histogram | — | `Update` callback duration |
| `warden_agentstore_ctx_abandoned_total` | counter | `op`, `phase` = `queued|callback` | ctx expired before commit |
| `warden_agentstore_snapshot_version` | gauge | — | monotonic |
| `warden_agentstore_snapshot_age_seconds` | gauge | — | now − builtAt of published snapshot |
| `warden_agentstore_snapshot_build_seconds` | histogram | `reason` = `boot|rebuild|divergence` | |
| `warden_agentstore_snapshot_served_total` | counter | `state` = `ok|suspect|stale` | reads by served state |
| `warden_agentstore_state` | gauge | `state` (one-hot) | `ok|suspect|degraded|unavailable` |
| `warden_agentstore_audit_runs_total` | counter | `result` = `clean|findings|error|timeout` | |
| `warden_agentstore_audit_duration_seconds` | histogram | — | |
| `warden_agentstore_audit_last_success_timestamp_seconds` | gauge | — | |
| `warden_agentstore_audit_findings` | gauge | `class` = `read|decode|integrity|divergence` | open findings |

Cockpit / daemon (`internal/daemon`, `internal/poller`, `internal/tui`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `warden_cockpit_request_duration_seconds` | histogram | `route` (templated), `status_class` | server-side, the 1 s poll routes especially |
| `warden_cockpit_requests_inflight` | gauge | `route` | |
| `warden_cockpit_stale_served_total` | counter | `route` | responses with `X-Warden-Store-State != ok` |
| `warden_cockpit_sse_clients` | gauge | — | |
| `warden_cockpit_sse_frame_seconds` | histogram | — | `send()` duration (List + tree + marshal) |
| `warden_cockpit_sse_notify_total` | counter | — | hub publishes (fan-out driver) |
| `warden_tui_poll_failures_total` | counter | `route`, `reason` = `timeout|http_5xx|decode` | client side (opt-in reporting; local TUI log otherwise) |
| `warden_poller_tick_seconds` | histogram | — | whole tick |
| `warden_poller_store_writes_total` | counter | `op` | writes per tick — the amplification input |

## 6. Dashboard and diagnostic contract

### 6.1 Diagnostics endpoint (spec-first: `openapi.yaml`, then `make generate`)

`GET /api/v1/store/diagnostics` — always 200, never scans, never takes the write lock
(must itself be safe to call during the incident). Additive; `store/health` is unchanged.

```json
{
  "checked_at": "RFC3339",
  "state": "ok|suspect|degraded|unavailable",
  "snapshot": {"version": 0, "age_ms": 0, "agents": 0, "closed": 0, "verified": true, "build_ms": 0},
  "lock": {"waiters": 0, "holder_op": "", "held_ms": 0,
           "slowest_holds": [{"op": "", "held_ms": 0, "at": ""}]},
  "ops": {"<op>": {"count": 0, "errors": 0, "canceled": 0, "p50_ms": 0, "p99_ms": 0}},
  "audit": {"last_run_at": "", "last_success_at": "", "last_result": "", "duration_ms": 0,
            "findings": [{"collection": "", "key": "", "class": "", "detail": ""}]},
  "stale": {"serving": false, "since": "", "max_age_s": 600},
  "ownership": {"pid": 0, "path": ""},
  "contract": 1
}
```

`lock.holder_op/held_ms/waiters` are the "who is holding the lock right now" answer
that was impossible to get during the incident; they are maintained with atomics, not
under the lock they describe. CLI mirror: `warden store diagnose [--json]`
(MCP parity: `store_health` gains the same payload). CLI help/website regeneration
(`make gendocs`) applies when the command lands.

### 6.2 Structured logs

One throttled line per window (existing `logStoreDegraded` pattern), generous threshold,
observe-only:
`agentstore: slow lock hold` `op= held_ms= waiters= holder_since=` at ≥ 250 ms;
`agentstore: queued call abandoned` `op= waited_ms= phase=`;
`agentstore: state transition` `from= to= cause=`.

### 6.3 Dashboard ("Agent store / Cockpit")

Row 1 — **Is the Cockpit healthy?** `warden_cockpit_request_duration_seconds` p50/p99 for
`/sessions`, `/events` frames, `/healthz`; stat tiles for `state`, `snapshot_age`,
`stale_served_total` rate.
Row 2 — **Contention.** `lock_wait_seconds` p99 by `class`, `lock_waiters`, `lock_hold_seconds`
p99 by `op` (top-5), `scans_total{caller=read|insert_name}` (must be flat 0 post-fix).
Row 3 — **Amplification.** `poller_store_writes_total` rate, `sse_notify_total` rate,
`sse_clients`, `sse_frame_seconds`.
Row 4 — **Integrity.** `audit_last_success` age, `audit_findings` by class, `audit_duration`.
Row 5 — **Cancellation.** `ctx_abandoned_total` by phase.

Alerts (page = red SLO, warn = early): `state != ok for 5m` (warn), `state=degraded` (page),
`lock_wait p99 > 100ms for 10m` (warn), `audit_last_success age > 2×interval` (warn),
`scans_total{caller=read} > 0` after the fix ships (regression).

Triage runbook (also the diagnostics reading order): `store/diagnostics` → if
`lock.holder_op` is set and `held_ms` large ⇒ contention (pre-fix) / stuck callback;
if `state=degraded` ⇒ `audit.findings`, then `warden repair agents --dry-run`;
if `stale.serving` ⇒ audit finding is the root cause, not the request path.

## 7. Gate map (what each downstream task must flip)

| Task output | Un-skip | Flip baseline |
|---|---|---|
| Snapshot read model + write-through (I-1…I-3, I-7…I-10) | `…ScanDoesNotBlockWriters`, `…WriteDoesNotBlockReaders`, `…GetIsPointRead` | `TestBaselineSlowScanBlocksWriters`, `…SlowWriteBlocksReaders`, `…ReadersSerialize`, `…ScanCountPerOperation`, `…UpdateFnRunsUnderLock`, daemon `…CockpitPollStalls…` |
| Cancellation (§4.4) | `…ContextBoundsQueuedCalls` | `TestBaselineContextIgnoredWhileQueued` |
| Stale/degraded + headers (§4.2) | `…DegradedServesLabelledStale` | `TestBaselineDegradedStoreFailsFastWithoutStale` |
| Auditor (§4.3) | `…AuditIsOffRequestPath` | — |
| Metrics + diagnostics (§5, §6) | new tests asserting metric names/labels against this table; `store_health_test.go` extended | — |

Each downstream change must also keep green: `contract_test.go`, `integrity_test.go`,
`internal/e2e/degraded_test.go`, `internal/daemon/store_health_test.go`.

## 8. Non-goals and open questions

- Not changing the engine (ScrivaDB pin stays `v1.4.0`), the on-disk format, the
  ownership flock, or offline repair.
- Not replacing the 1 s TUI poll with push in this track (SSE already exists); the
  contract makes polling cheap rather than removing it. A TUI move to SSE is a
  separate, additive follow-up.
- Other ScrivaDB-backed stores (pipelines, plans, projects, …) have the same
  single-mutex shape; out of scope, tracked as a follow-up audit once the pattern is
  proven here.
- Open: whether `Update` callbacks should receive a copy-on-write value (preferred, I-4)
  or keep mutating in place under the write lock (cheaper, smaller diff). The
  contract only requires that readers never wait on it.
- Open: memory cost of a full in-memory snapshot at 10k+ agents (events slices are the
  bulk). Mitigation if needed: snapshot holds a *summary* projection and `Get` of
  full events falls back to an engine point read — still O(1), still lock-free.

## 9. Diagnostic seams (shipped with this record)

`internal/agentstore/seams.go`: `SetScanSeam` (called inside `scanVerified`, under
`mu`) and `SetWriteSeam` (called by `Insert`, `Update`, `Archive`, `UpdateStatusIf`,
`FinalizeExit`, `Delete` right after taking `mu`). Both are a nil atomic-pointer load in
production — no behavior change. They are process-global; tests that install them must
not run in parallel and must use the returned restore func. They are removed or
repurposed (e.g. to inject slow audit/rebuild) by the snapshot task.

## 10. Reproducing the trace

```sh
grep -rEn '\.(store|sstore|st|sessions|agents)\.(Get|GetByNameOrID|List|ListClosed|ListClosedDegraded|Insert|Update[A-Za-z]*|FinalizeExit|Archive|Delete|AppendEvent[A-Za-z]*|SetRestart|StampCompact|SetForceCompact|ClearWorktree|SetRateLimit|ClearRateLimit|SetSessionID|Ping|Create|Spawn)\(' \
  --include='*.go' internal cmd | grep -v _test.go | grep -v '^internal/\(planstore\|pipeline\|tui\|cli\|client\)/'
```

Counts in §2 come from this grep on commit `fbf575a6`; re-run it when the call-site set
changes (a new reader on a Cockpit-hot path must be added to §2.3).
