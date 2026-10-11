# Warden DB transaction boundary (`internal/dbtx`)

**Status:** implemented, 2026-10-10. Run `ap-54999c983c26`, plan 02, task `transaction-boundary`.
**Builds on:** [ScrivaDB XTx capability audit](2026-10-10-scrivadb-cross-collection-transaction-capability-audit.md) (G-numbers below refer to its §4).
**Used by:** [collection contract §6.3](2026-10-10-warden-db-collection-contract.md) atomic list; next task `invariant-workflows` converts owners onto it.

`internal/dbtx` is the **only** package that may use `engine.CommitXTx`, `XTxStatus`, `XTxOp`
(G13; `TestImportFence`). It is internal-only: no CLI/README/site change.

## Hard rules

- Atomic commit or a typed failure. **No** sequential single-collection fallback, no compensate-after,
  no partial write set. `ErrXTxUnsupported`/`ErrFormatTooNew`/`ErrReadOnly` surface as `ErrAtomicUnavailable`.
- Transactions cover internal DB state only. External effects (tmux, git, files, hub) follow the
  operation-journal pattern: intent tx → effect → outcome tx.
- A `Tx` holds no engine resource; it is an in-memory typed op list. No implicit long-running tx.

## API

```go
b, _ := dbtx.New(scrivaDB, opts...)              // or NewWithEngine(Engine) (test seam)
a, _ := dbtx.Bind[T](b, "agents", codec)         // declared + must already exist (G8)
tx, _ := b.Begin(ctx)                            // nested Begin → ErrNestedTx (G2)
id, _ := a.Insert(tx, v)                         // id from ReserveID only (G3)
_ = a.Update(tx, id, v, expectedRev)             // 0 = unconditional
_ = a.Delete(tx, id, expectedRev)
res, err := tx.Commit(ctx, "<operation-id>/<step>")  // exactly one CommitXTx call
_ = tx.Rollback()                                // idempotent; safe to defer

res, err := b.Run(ctx, intent, func(ctx, tx) error { /* re-read + restage */ })  // bounded retry
```

- `Coll.Get` is a plain non-transactional read returning `Versioned{ID, Rev, Value}`; feed `Rev` to `expectedRev`.
- `Result.RevsAuthoritative == false` for replayed / status-confirmed commits: `Rev` is zeroed (C3) — re-read.
- `Option`s: `WithObserver`, `WithMaxAttempts` (4), `WithMaxStagingAge` (2m), `WithCommitTimeout` (30s when ctx has none),
  `WithDeadlineGrace` (2s), `WithResolveWait` (5s), `WithBackoff`.
- Observer seam: one `Event{Intent, Key, TxID, Ops, Participants, Outcome, Resolved, Duration}` per `Commit`
  (the `observability` task attaches metrics/audit here).

## Idempotency key (G5)

`key = intent + "/" + hex(sha256(json(ops)))[:24]`, ≤128 bytes. The same key therefore never names a different
op set. An identical re-commit whose key is already COMMITTED makes **no** engine call and returns a replayed
result. Inserts reserve fresh ids per attempt, so cross-attempt dedup of inserts is the operation journal's job,
not the key's.

## Error classes and retry (G6)

All failures are `*dbtx.Error{Class, Key, TxID, Outcome, SafeToRetry, Attempts}`; `dbtx.Classify(err)` maps any error.

| Class | Engine cause | Retried by `Run`? |
|---|---|---|
| `conflict` | `ErrXTxConflict` (ExpectedRev, missing target, unique, dup op) | yes, after rebuild |
| `exhausted` | `ErrResourceExhausted`/`ErrQuotaExceeded` | yes (bounded) |
| `durability` | `ErrXTxDurability` (journal/segment poisoned until restart) | yes (bounded; will usually keep failing) |
| `too_large` | `ErrXTxTooLarge`, or pre-flight: >1000 ops, >16 participants, >64 MiB est., key >128 B | no — nothing reaches the engine for pre-flight |
| `outcome_unknown` | `ErrXTxOutcomeUnknown`, `ErrXTxInProgress`, commit deadline | only if resolved ABORTED/UNKNOWN **and** the engine said OutcomeUnknown (not in-progress, not deadline) |
| `collection_not_found` | missing participant (`Bind` time) | no |
| `internal` | everything else, incl. untyped errors and all misuse sentinels | no (fail closed) |

Retry needs both `Class.Retryable()` and `engine.XTxRetrySafe(err)`; each attempt calls the rebuild callback
afresh — never a blind replay.

## Outcome resolution (G4, G7)

- `ErrXTxOutcomeUnknown` → poll `XTxStatus(key)` (PENDING waits ≤ `ResolveWait`):
  COMMITTED ⇒ success (not authoritative revs); ABORTED/UNKNOWN ⇒ `SafeToRetry` (rebuild re-reads, so a
  secretly-durable commit is observed as no-change via `ErrNoChange`); PENDING ⇒ error, never success.
- `CommitXTx` has no ctx, so it runs in a goroutine. On ctx deadline the boundary waits `DeadlineGrace` for it,
  then checks status: COMMITTED ⇒ success; otherwise `ErrCommitDeadline` (class `outcome_unknown`, not retryable —
  the goroutine may still commit). Callers resolve later with `b.Outcome(key)`.

## Owner usage

1. Bootstrap/migration creates collections + indexes first; owners `Bind` once.
2. Hold the per-domain writer lock (G1), read, validate FKs/restrictions in the owner, stage, commit.
3. Pass `<operation-journal-id>/<step>` as intent. Prefer `Run` with a rebuild that re-reads and may return
   `ErrNoChange`.
4. Keep XTx off hot streams (usage/spend/events; G11). Split >1000-op cascades into journal-driven steps (G9).

## Representative workflows

Fixtures (not a production migration) in `internal/dbtx/workflows`: each workflow is ONE `Run` transaction with a
rebuild callback (ExpectedRev fencing, idempotent via `ErrNoChange`), plus an exported checker valid on any state.
`workflows.All()` returns `{Name, Seed, Run, Check}` for `crash-validation`; `Setup(db)` creates the shared schema.
Every record has a unique logical `id`; external effects are only `operations` journal rows (intent in the
transaction, outcome in a second one).

| Workflow | Participant collections | Invariant | Checker |
| --- | --- | --- | --- |
| project-child-creation | projects, agents, project_views | every agent has an existing, open project; `live_agents` equals live rows; view matches project | `CheckProjectChildren` |
| autopilot-startup | autopilot_runs, plans, projects, agents, operations, events | one live run per plan (unique `live_plan`); run, plan binding, agent and launch-intent op exist together | `CheckAutopilotStartup` |
| pipeline-linkage | pipelines, pipeline_jobs, plans, events | pipeline and jobs link to a ready plan both ways; a plan links to at most one pipeline | `CheckPipelineLinkage` |
| archive-restore | agents, agents_archive, projects, project_views | an agent is in exactly one of live/archive; counters equal rows; restore is the exact inverse (delete+insert move) | `CheckArchive` |
| project-closure | projects, agents, agents_archive, plans, autopilot_runs, project_views | a closed project has no live children, runs or open plans; closure over `MaxOps` fails typed `too_large` (caller-driven drain, never partial) | `CheckProjectClosure` |
| projection-update | projects, project_views | `project_views` row equals the project's derived state and version, in the same tx as every project write | `CheckProjection` |
| audit-event-append | plans, events | plan `Revision` equals its event count; event ids `<plan>@<rev>` are contiguous | `CheckAuditTrail` |

Not covered: the R2 job↔agent binding, metrics (`observability`). Crash/fault/concurrent matrices: see below.

## Crash and fault validation

Test-only suite: `internal/dbtx/crashtest` (no production change). Every scenario iterates `workflows.All()` and
asserts **all-or-nothing**: after the fault and a reopen the data is exactly the PRE or exactly the POST snapshot,
every workflow checker holds, `Outcome(key)` agrees with the state (POST ⇒ COMMITTED, PRE ⇒ not COMMITTED/PENDING),
reopening is idempotent, and re-running the workflow converges to POST. A reported success is never absent after
reopen, and a final failure is never present unless it was `outcome_unknown`.

| # | Scenario | Test | Method |
|---|---|---|---|
| 1 | Process crash before/during/after commit | `TestRealProcessCrash`, `TestFirstCommitCrashWindow` | real `SIGKILL` of a helper subprocess at: staged-before-commit, ack-after-commit, first segment growth, first journal growth, ± sub-ms delays, seeded random delays |
| 2 | Failed commits, every error class | `TestInjectedCommitFaults` | `NewWithEngine` seam: conflict, exhausted, durability (once / always), too_large, collection_not_found, untyped, outcome_unknown resolving COMMITTED / ABORTED / UNKNOWN / PENDING, in-progress, deadline + late commit |
| 3 | Torn / damaged journal | `TestJournalDamage` | crash-image copy (taken with the DB still open), then truncate / flip / delete / zero `xtx.journal` |
| 4 | Conflicts | `TestConflictsRaceBetweenRebuildAndCommit` | `Interfere` in `BeforeCommit`: once → converges; single attempt → typed conflict; every attempt → `exhausted`; state never changes on failure |
| 5 | Concurrent writers | `TestConcurrentSameWorkflow`, `TestConcurrentAllWorkflows`, `TestInverseParticipantOrderNoDeadlock` | N goroutines same workflow; all workflows at once vs a sequential reference; two writers staging two collections in opposite order on contended fenced records (bounded timeout, counters exact, pairs intact) |
| 6 | Reopen idempotence | every `verifyReopened` | several close/open cycles, byte-stable snapshot |

**Result:** 7 workflows × all scenarios pass under `-race`; the whole package runs in ~30 s. Crash outcomes are a
mix of PRE and POST, never MIXED.

Run: `go test -race ./internal/dbtx/crashtest` (default, CI-sized). Soak (≈60 extra random kill points per workflow,
more reopen cycles, more concurrent goroutines): `WARDEN_CRASH_SOAK=1 go test -race -timeout 30m ./internal/dbtx/crashtest`.

### Observed journal-damage behaviour (scenario 3)

Scriva exposes no crash hooks, so crash points come from real kill timing plus file-level damage of a crash image.

| Damage to `xtx.journal` (COMMIT record just written) | Reopen |
|---|---|
| tail truncated 1 byte / mid-record / header only / record removed | opens; state **PRE** (torn tail dropped, presumed abort) |
| garbage appended after a complete COMMIT record | opens; state **POST** |
| byte flipped inside the COMMIT record | **fails closed**: `ErrXTxJournalCorrupt` (checksum mismatch) |
| file deleted while stamped entries exist | **fails closed**: `ErrXTxJournalMissing` |
| file truncated to 0 | opens; state **PRE** (every un-checkpointed commit is rolled back, including earlier ones — destroying the journal is a rollback, by design) |

After a *clean* close the engine checkpoints and the stamps are gone, so journal damage then no longer affects data.

### Findings and residual risks

- **F1 — upstream scriva defect (reproduced; no Warden production change).** A `SIGKILL` between creation of
  `xtx.format` and `xtx.journal` (i.e. during the database's **first-ever** cross-collection commit) leaves a root
  where `scriva.Open` succeeds but every `db.Collection(name)` on the façade fails permanently with
  `ErrXTxRecoveryRequired` ("open via engine.Open"): the façade only supplies recovery decisions when the journal
  exists. Data stays all-or-nothing (PRE) at the engine level; the store is merely unusable via the façade until a
  journal exists. Pinned by `TestFacadeFormatWithoutJournal` (skips citing F1 while present; asserts usability once
  fixed). `TestFirstCommitCrashWindow` tolerates it and verifies the engine-level state. Mitigation to adopt in the
  owner conversion (not done here): bootstrap the XTx root (one warm commit) at store creation, before any owner
  workflow, or fix upstream (`CollectionWithConfig` should create/recover the journal).
- **I2 — no cross-collection read snapshot (audit gap).** An ungated reader scanning several collections during a
  commit can see a torn *view*; the suite samples checkers mid-run **gated** (valid probe, asserted) and **ungated**
  (informational; violation counts are logged, not asserted).
- **`Interfere` is an unfenced competing write**; running it concurrently with a commit races its own
  read-modify-write and can clobber the committed counters (test artifact, not a boundary defect). It is therefore
  used only inside `BeforeCommit`.
- **SIGKILL is not power loss.** A killed process keeps its page-cache writes, so unsynced-write loss and
  reordering are not exercised; journal truncation models torn tails only. fsync-ordering faults need an upstream
  fault-FS hook (unexported today).
- Scenario matrix is fixed-seed (`20261010`) for reproducibility; soak widens coverage, it does not prove absence.

## Observability

The transaction boundary exports metrics and audit events through the `Observer` interface (`type Observer interface{ Observe(Event) }`).
Telemetry is non-blocking and safe by construction: observer panics or failures never disrupt transactions (`safeObserve`).

### Metrics (`dbtx.MetricsCollector`)

The in-memory collector (`dbtx.NewMetricsCollector`) aggregates point-in-time snapshots (`Snapshot()`):

- **Transaction latency histogram (`Histogram`)**: Fixed 12-bucket histogram (`[1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000] ms` + `+Inf`).
  Computes `Count`, `SumMs`, `MinMs`, `MaxMs`, `P50Ms`, `P95Ms`, and `P99Ms` with zero runtime allocations during observation.
- **Conflict counters (`conflicts_total`)**: Monotonically increasing count of `ClassConflict` errors, tracked globally and broken down per normalized intent.
- **Rollback counters (`rollbacks_total`)**: Tracks explicit rollbacks (`tx.Rollback()` on uncommitted staged writes), build-stage errors in `Run`, and failed commit rollbacks.
- **Recovery metrics (`recoveries_total`)**: Incremented when a transaction outcome is established via status lookup/recovery (`ev.Resolved == true`).
- **Abandoned operation metrics (`abandoned_total`)**: Tracks operations aborted before completion, timed out (`ErrCommitDeadline`), expired staging (`ErrStagingExpired`), or unresolved `OutcomeUnknown`.
- **Error classification (`by_class` / `errors_total`)**: Breakdown across all closed failure classes (`conflict`, `exhausted`, `durability`, `too_large`, `outcome_unknown`, `collection_not_found`, `internal`).

### Safe Cardinality Guarantees

To eliminate cardinality explosion and unbounded memory growth:

1. **Intent normalization (`NormalizeIntent`)**:
   - Known static workflow intents (`project-child-creation`, `autopilot-startup`, `pipeline-linkage`, `archive-restore`, `project-closure`, `projection-update`, `audit-event-append`) are preserved verbatim.
   - Dynamic operation journal intents (`<id>/<step>`, e.g. `op-01928374/launch`, `plan-abcd/step`) are mapped to canonical family names (`operation/launch`, `plan/step`, `agent`, `project`, `pipeline`, etc.).
   - Pure hex/numeric hashes and UUIDs are mapped to `"custom"`.
   - All labels are sanitized and length-capped (`maxIntentLen = 48`).
2. **Strict label set bounding (`maxIntentBuckets = 64`)**:
   - The metrics collector caps the distinct intent buckets to 64. Any unrecognized or overflow intents beyond the cap are routed to `"other"`.
   - Memory consumption of the metrics collector is strictly $O(1)$.
3. **Closed outcome vocabulary (`NormalizeOutcome`)**:
   - Strictly bounded to `"committed"`, `"rolled_back"`, `"abandoned"`, `"no_change"`, the 7 typed `Class` strings, and `"other"`.
4. **Never raw payloads or secrets in labels**:
   - Raw transaction payloads, cryptographic key hashes, user inputs, and credentials NEVER enter metrics labels.

### Audit Events (`dbtx.AuditObserver`)

Significant lifecycle events are written best-effort to `~/.warden/audit.jsonl` using `dbtx.NewAuditObserver(auditWriter)` (or configured via `dbtx.WithAudit(writer)` / `dbtx.DefaultObserver(writer)`):

| Action | Constant | Trigger | Target | Details |
|---|---|---|---|---|
| `tx_recovery` | `audit.ActionTxRecovery` | Outcome established via status lookup / recovery (`Resolved: true`) | TxID or intent | `intent`, `outcome`, `tx_id`, `duration`, `ops`, `resolved` |
| `tx_abandoned` | `audit.ActionTxAbandoned` | Transaction aborted, timed out, expired staging, or `outcome_unknown` | TxID or intent | `intent`, `outcome`, `tx_id`, `duration`, `ops` |
| `tx_outcome` | `audit.ActionTxOutcome` | Severe durability errors (`ClassDurability`) or oversized limit breaches | TxID or intent | `intent`, `outcome`, `tx_id`, `duration`, `ops` |

All audit details pass through `audit.SafeDetail` and `audit.RedactSecrets` to strip forbidden keys (tokens, authorization headers, passwords) and redact sensitive values before persistence.

## Deferred

Production owner conversions (the fixtures above are the template), re-pin to a tagged scriva (G13).

