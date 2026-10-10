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

## Deferred

Metrics/audit events (`observability`), owner conversions (`invariant-workflows`), crash/fault matrix
(`crash-validation`), re-pin to a tagged scriva (G13).
