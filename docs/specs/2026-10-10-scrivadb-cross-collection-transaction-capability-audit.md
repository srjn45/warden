# ScrivaDB cross-collection transaction capability audit and pin decision

**Status:** audit, 2026-10-10. Docs + dependency pin only; no Warden wrapper code.
**Run:** autopilot `ap-54999c983c26`, plan 02 (ScrivaDB cross-collection transaction adoption), task `capability-audit`.
**Feeds:** task `transaction-boundary` (narrow Warden transaction abstraction).
**Consumers of the guarantee:** [collection contract §6.3](2026-10-10-warden-db-collection-contract.md), [operation journal](2026-10-10-warden-db-operation-journal-and-reconciliation.md), [migration acceptance](2026-10-10-warden-db-schema-2-migration-acceptance.md).

All upstream references are to the ScrivaDB repo (`github.com/srjn45/scriva`) at commit
`4ca8d00d1197` (`fix(engine): remove unused transaction helper (#154)`, on `main` after `v1.4.0`).
`74123d0` (current `main` HEAD) differs only by a `.gitignore` change.

Status legend: **VERIFIED-IN-CODE** (read in code at the pinned commit; named tests exist and pass),
**DESIGNED-NOT-YET-SHIPPED** (specified in the upstream design doc, absent in code),
**UNKNOWN/GAP** (not specified, or specified and contradicted/insufficient for Warden).

## 1. Pin decision

| Question | Answer |
|---|---|
| Does `v1.4.0` (Warden's previous pin) contain XTx? | **No.** `git diff v1.4.0..main` adds `engine/xtx_*.go`, `internal/xtxspec`, and the design docs. |
| Is there a tagged release with XTx? | **No.** Latest tag is `v1.4.0`. No tag after it. |
| Is the API reachable from untagged `main`? | **Yes**, as the *engine-level* API: `(*engine.DB).CommitXTx`, reachable from the embedded façade via `scriva.DB.Engine()` (`scriva.go:261`). There is **no façade-level XTx API**, no typed read-your-writes transaction object, and no isolation beyond what is listed in §3. |
| Pin | `github.com/srjn45/scriva v1.4.1-0.20261010192719-4ca8d00d1197` (= `4ca8d00d1197`, pseudo-version via `go get github.com/srjn45/scriva@4ca8d00`). |
| Why this commit | It is the last functional commit that contains the coordinator, commit path, recovery, snapshot barrier, verify/repair gates and compaction evidence retention (`432ba38`, `4ca8d00`). `74123d0` is not functionally different; a functional commit is a clearer pin than a housekeeping one. |
| Why pin an untagged commit at all | The `transaction-boundary` task needs the real engine types to compile and test against; a hard "no pin" would block the plan. No Warden production data format changes with this bump (see "What the bump does to existing data"). |
| Re-pin obligation | **Must be re-pinned to a tagged release containing XTx once cut** (expected `v1.5.0`). Pseudo-versions must not ship in a Warden release that writes schema-2 transactions to user data. The upstream autopilot run `04-cross-collection-transactions-api-isolation` (0/3 tasks landed at audit time) is the likely source of the public embedded API and isolation; any API it adds may supersede §3 and is expected to be additive. |
| Never | Never pin `v1.2.0` (see repo memory/CLAUDE guidance; corruption defects fixed in `v1.2.1`+). |

### What the bump does to existing data

`Open` now runs `recoverCoordinator` (`engine/db.go:101`, `engine/xtx_recovery.go:46`) for **every**
root. For a legacy root with no `xtx.format`/`xtx.journal` it takes the legacy path and writes
nothing; XTx bytes appear only after the first `CommitXTx` (`ensureXTxJournal`, `engine/db.go:307`).
A root that has *never* run `CommitXTx` stays byte-identical to `v1.4.0`
(upstream design §0, invariant "no-XTx byte identity"; tests `TestXTxLegacyRootUntouched`, `TestXTxFormatGate` in `engine/xtx_gate_test.go`).

**Downgrade hazard (Warden must treat as one-way):** once `CommitXTx` has run, a `v1.4.0` binary
(and any pre-fence binary) opens the root *fail-closed* with a corrupt-entry error and its `Repair`
could quarantine stamped segments (upstream design §11.2 "Known residual"). Warden `warden-db`
backup/rollback (`wd rollback`) must restore the pre-first-XTx data directory, not just the binary.

## 2. Exact Go API surface Warden may depend on

All in package `github.com/srjn45/scriva/engine` unless noted. Pinned-commit file:line.

### 2.1 Commit

```go
// engine/xtx_commit.go:12-37
type XTxOp struct {
    Collection  string         // must exist; must not start with "xtx."
    Op          store.Op       // store.OpInsert | store.OpUpdate | store.OpDelete
    ID          uint64         // caller-supplied record id (see Collection.ReserveID)
    Data        map[string]any // insert/update body
    ExpectedRev uint64         // update/delete only; 0 = no check
    ExpiresAt   int64          // 0 = default TTL / keep existing
}
type XTxOpResult struct { Collection string; ID uint64; Rev uint64; Op store.Op }
type XTxResult   struct { TxID string; Ops []XTxOpResult }

// engine/xtx_commit.go:42
func (db *DB) CommitXTx(key string, ops []XTxOp) (*XTxResult, error)
```

`key` is the idempotency key (≤128 bytes, `xtxMaxKeyLen`, `engine/xtx_journal.go:50`; empty = no dedupe).
There is **no** `Begin`/`Rollback`/`Stage*`: a transaction is a flat, fully-materialised op list passed
to one call. "Rollback" before commit is "do not call"; after a failed call the library has already
undone or never applied it (§3.1).

### 2.2 Status

```go
// engine/db.go:322
func (db *DB) XTxStatus(txOrKey string) (XTxStatus, string) // status, txid
type XTxStatus string // XTxCommitted | XTxAborted | XTxPending | XTxUnknown | XTxExpired
```
(`engine/xtx_errors.go:94-102`.) Argument is a txid (`<16hex>-<16hex>`) or an idempotency key.
Returns `XTxUnknown` when no journal has been created yet.

### 2.3 Collections (outside the transaction)

`DB.CreateCollection(name)`, `DB.CreateCollectionWithDefaultTTL`, `DB.DropCollection`,
`(*Collection).ReserveID() uint64` (`engine/collection.go:1449`), and via the façade
`scriva.DB.Collection(name, opts...)` (`scriva.go:221`) / `scriva.DB.Engine()` (`scriva.go:261`).
Reads stay on `*engine.Collection` (`Get`, secondary-index queries, scans); they are **not** part
of any transaction (§3.2).

### 2.4 Errors (sentinels for `errors.Is`; `*engine.XTxError{Tx, Err, Cause}` wraps them, `engine/xtx_errors.go:67-91`)

| Sentinel | Meaning at the pinned commit | `engine.XTxRetrySafe` |
|---|---|---|
| `ErrXTxConflict` | S0 validation failed: missing/expired update or delete target, `ExpectedRev` mismatch, unique-index violation (also wraps `ErrDuplicateKey`), duplicate `(collection,id)` in one tx (wraps `ErrXTxDuplicateOp`), encryption seal failure | true |
| `ErrResourceExhausted` / `ErrQuotaExceeded` (alias) | quota breach checked for **every** participant before any write | true |
| `ErrXTxTooLarge` | > 1000 ops, > 16 participants, > 64 MiB estimate, or key > 128 B; rejected before any write | **false** |
| `ErrXTxDurability` | S1 append or S2 fsync failure, or poisoned participant segment; runs rolled back, tx aborted; participant/journal poisoned until restart | true (but needs restart if poisoned) |
| `ErrXTxOutcomeUnknown` | S4 journal fsync failed; the commit record may or may not be durable | **false** — resolve via `XTxStatus` / same key only |
| `ErrXTxInProgress` | same key concurrently executing in this process | false |
| `ErrCollectionNotFound` | a participant does not exist | not in list (treated unsafe) |
| `ErrReservedName` | collection name starts with `xtx.` | not in list |
| `ErrReadOnly` | follower | not in list |
| `ErrXTxIncomplete`, `ErrXTxJournalCorrupt`, `ErrXTxJournalMissing`, `ErrXTxDecisionConflict`, `ErrXTxUnsupported`, `ErrFormatTooNew`, `ErrXTxFormatCorrupt`, `ErrXTxRecoveryRequired` | open-time / integrity failures; `Open` fails closed | n/a |
| plain `errors.New("engine: cannot commit empty transaction")`, `empty collection name`, `unknown op` | **untyped** — no sentinel (`engine/xtx_commit.go:47,63,263`) | false (fail closed) |

## 3. The nine dimensions

### 3.1 Atomicity — **VERIFIED-IN-CODE** (single DB root, in-process)

**Guarantee.** For one `CommitXTx` over 2..N collections of **one `engine.DB`**
(one data dir, one process, one `LOCK`), after commit returns `nil` every op is present; after any
error other than `ErrXTxOutcomeUnknown` none is present; after a crash, every *acknowledged* tx is fully
present and every unacknowledged tx is fully absent (presumed abort).

**Evidence.**
- Protocol: redo-only; stamped invisible entries in each participant's active segment, one durable
  `COMMIT` record in root `xtx.journal` is the single commit point. `docs/design-cross-collection-transactions.md` §0, §2, §6.2 (S0–S7), §13.
- Code: `engine/xtx_commit.go` — S0 validate under all locks (`:131-270`), S1 stamped append (`:300-325`),
  S2 fsync (`:328-337`), S3+S4 `j.commitPrepared` (`:341-352`), S5 apply to indexes (`:354-420`).
- Pre-apply rollback truncates runs on append/fsync failure (`rollbackRuns`, `:288`).
- Tests: `TestCommitXTx_TwoCollectionCommit`, `_ThreeCollectionsWithDeletes`, `_ValidationErrorsNoWrites`,
  `_S1AppendFailureRollback`, `_FaultInjectedFsyncPoisonsSegment`, `_S4FsyncFailurePoisonsJournalOutcomeUnknown`
  (`engine/xtx_commit_test.go`); recovery truth table `engine/xtx_recovery_test.go`.

**Not covered.**
- Multi-process, multi-root, or networked atomicity (design §0 non-goal).
- Atomicity of **non-engine** side effects (tmux, git, files, hub calls): out of scope by definition;
  stays with the operation journal (hard constraint, §5).
- **Gap A1** — Existing-id insert is not validated: `OpInsert` in S0 (`engine/xtx_commit.go:187-209`) never
  checks `c.index.Get(op.ID)`, so inserting an id that already exists is accepted and overwrites with `rev=1`.
  Warden must obtain ids via `ReserveID` and treat "create" as unique-index-guarded, never rely on the engine
  rejecting a duplicate id.
- **Gap A2** — Warden's contract R9 (unique/partial-unique indexes) is enforced only through the *existing*
  per-collection unique secondary index (`txCheckUnique`, `:260`). A partial-unique or cross-collection
  uniqueness must be modelled as a unique index on a derived key or as a guard record.

### 3.2 Isolation — **VERIFIED-IN-CODE (narrow)** / **UNKNOWN/GAP (read side)**

**Guarantee (what is actually provided).**
- *Serializable write sets*: all participants' `c.mu` are write-locked in bytewise-ascending name order from
  S0 through S5 under `DB.mu.RLock`; two XTx and any single-collection writer to the same collection are totally
  ordered; canonical order makes participant sets deadlock-free (design §6.1, §9.1; `engine/xtx_commit.go:121-146`,
  `TestCommitXTx_CanonicalOrderingDeadlockFree`).
- *Per-collection read-committed visibility*: a reader of one collection sees a tx entirely or not at all
  (S5 happens under that collection's write lock).
- *Optimistic concurrency*: per-op `ExpectedRev` is checked in S0 under the lock (exact, no TOCTOU).

**NOT provided (must be designed around):**
- **Gap I1 — no transactional reads.** `CommitXTx` takes a *pre-built op list*. Warden cannot read inside the
  transaction, hold a snapshot, or have the engine validate a *read set*. Every "validate FK / recompute status /
  restriction check, then write" flow in the collection contract §6.3 (agent create + parent FK validation,
  project delete restriction check, pipeline status recompute) has a check-then-act window between Warden's
  read and the engine's S0 lock.
- **Gap I2 — no cross-collection read snapshot.** A reader reading A then B can see A-before and B-after
  (design §9.1 "Not guaranteed"; an optional DB-level read gate is "not decided", §15).
- **Gap I3 — no FK/constraint engine.** Foreign keys and cross-collection unique constraints are an explicit upstream
  non-goal (design §0). The collection contract's "engine rejects violations (transaction abort)" (§6.1) is **not**
  satisfied by ScrivaDB; Warden's own layer must enforce strong FKs.
- **Gap I4 — read-dependency fencing is not offered.** `ExpectedRev` is only available on an op that *writes* the
  record. To fence a parent that is merely read, Warden must include a no-op/"touch" update of that parent in the
  transaction with `ExpectedRev` (bumps its rev; acceptable only for rarely-written parents) or serialize
  through a single Warden-level writer lock for the relevant domain.

### 3.3 Durability — **VERIFIED-IN-CODE**

**Guarantee.** When `CommitXTx` returns `nil`: every participant's stamped run was `fsync`ed
(S2) **unconditionally**, regardless of the collection's `SyncMode`, and the `COMMIT` record was
`fsync`ed (S4) before S5 and before return. Ordering `fsync(all runs)` → `append(COMMIT)` → `fsync(journal)` → apply → ack
is the protocol (design §6.3, §6.2). New root files (`xtx.format`, `xtx.journal`) are made durable
(file + directory fsync) before the first stamped byte (`engine/xtx_journal.go:654-760`).

**fsync failure semantics.** A failed S2/S3 fsync poisons the segment (`c.active.poisoned = err`,
`engine/xtx_commit.go:334`) and returns `ErrXTxDurability`; a failed S4 fsync returns
`ErrXTxOutcomeUnknown` and poisons the journal. Neither is retried in-process; restart is the only
recovery (design §6.3 rule 3).

**Evidence.** `engine/xtx_commit.go:328-352`; tests `_FaultInjectedFsyncPoisonsSegment`,
`_S4FsyncFailurePoisonsJournalOutcomeUnknown`, crash fixtures in `internal/xtxspec/boundaries.go`
(each write/fsync/rename with deterministic post-crash states).

**Caveats.**
- Cost: n+1 fsyncs per XTx (n participants + journal). Warden must not put XTx on hot per-event paths
  (usage/spend/metrics streams stay single-collection, per contract class classification).
- The S2 fsync also flushes earlier un-synced entries of that segment; it never *relaxes* another
  participant's own SyncMode.
- Storage that lies about `fsync` is outside the guarantee (as for single-collection writes).
- **Gap D1** — Journal group commit across concurrent XTx is permitted by the design (§6.2 S4) but is **not**
  implemented: each XTx pays its own journal fsync. Throughput cap: serial.

### 3.4 Conflict behavior — **VERIFIED-IN-CODE** (optimistic, fail-fast, no blocking)

| Condition | Result | State on disk | Retry |
|---|---|---|---|
| target of update/delete missing or TTL-expired | `ErrXTxConflict` | none | after re-read |
| `ExpectedRev != 0 && actual != ExpectedRev` | `ErrXTxConflict` (message names collection/id/expected/actual) | none | after re-read |
| unique secondary index violation | `ErrXTxConflict` wrapping `ErrDuplicateKey` | none | after re-read |
| same `(collection,id)` twice in one tx | `ErrXTxConflict` wrapping `ErrXTxDuplicateOp` | none | fix caller |
| quota | `ErrResourceExhausted`/`ErrQuotaExceeded` | none | after freeing space |
| same key concurrently in flight | `ErrXTxInProgress` | none | wait / status |
| lock contention | **blocks** on `c.mu` (no timeout, no `ErrXTxBusy`) | — | n/a |

All S0 checks run under the participant locks, so a rejection is exact and writes nothing
(`TestCommitXTx_ValidationErrorsNoWrites`, `_UniqueConstraintConflict`, `_QuotaExceeded`,
`_ConcurrentInProgressKey`). Conflicts are returned by value; there is no automatic retry in the library.

**Gaps.**
- **Gap C1 — errors lose structure.** `ErrXTxConflict` details (collection, id, expected, actual) exist only
  in the message string; no typed field. Warden's classifier must treat `errors.Is(err, ErrXTxConflict)` as the
  whole signal.
- **Gap C2 — lock waits are unbounded.** There is no `context.Context` or timeout on `CommitXTx`; a wedged
  participant lock (e.g. a long compaction holding `c.mu`) blocks callers indefinitely. Warden must run
  commits off the request path with its own deadline and must not hold its own locks across the call.
- **Gap C3 — idempotent replay is lossy** (`engine/xtx_commit.go:104-111`): a replay of a committed key returns
  `Rev: 1` for every op regardless of actual revs and does **not** compare the replayed ops against the
  original. Warden must never rely on `XTxResult.Ops[i].Rev` from a *replayed* commit and must derive keys
  that cannot collide across distinct payloads (e.g. `<operation>:<entity id>:<intent hash>`).

### 3.5 Collection creation — **UNKNOWN/GAP** (not transactional)

- `CommitXTx` resolves every participant from `db.collections` under `DB.mu.RLock` and returns
  `ErrCollectionNotFound` **before any write** if one is missing (`engine/xtx_commit.go:133-137`).
- `CreateCollection`/`DropCollection` take `DB.mu.Lock` and are separate, non-transactional,
  non-atomic with respect to any XTx (they wait for an in-flight XTx, never join one).
- `CreateCollection` rejects the reserved `xtx.` prefix (`engine/collection.go:377-378`).

**Consequences for Warden.** All schema-2 collections and indexes must be created by an idempotent
bootstrap/migration step *before* any transaction; there is no "create collection in the same
transaction as its first record". A crash between `CreateCollection` and the first XTx leaves an empty
collection, which is benign. `DropCollection` inside a live domain (e.g. a rollback path) is **not**
transactional with data cleanup and must not be used as a "transactional delete".
Verified by absence: no API in `engine/xtx_*.go` creates or drops a collection.

### 3.6 Recovery — **VERIFIED-IN-CODE** (presumed-abort, fail-closed on journal damage)

**Guarantee.** `engine.Open` runs `recoverCoordinator` (`engine/xtx_recovery.go:46-389`) *before*
indexes are trusted: parse journal (torn tail truncated, complete-record corruption fails closed),
discover stamped runs in active-segment tails, resolve per the §7.2 truth table, truncate undecided runs
and append `ABORT{why:recovery}`, rebuild/tail-replay indexes under one shared visibility predicate.

| Situation at open | Result |
|---|---|
| runs present, no `COMMIT` | presumed ABORT: tail truncated, `ABORT` journalled |
| `COMMIT` + every participant run complete, digests match | COMMITTED, redone into indexes |
| `COMMIT` but a participant run missing/short/digest mismatch | `ErrXTxIncomplete`, Open **refuses** (never commits a subset, never aborts a decided tx) |
| journal corrupt in the middle / duplicate-conflicting decisions | `ErrXTxJournalCorrupt` / `ErrXTxDecisionConflict`, Open refuses |
| stamped entries present, journal missing | `ErrXTxJournalMissing`, Open refuses |
| reserved root directory `xtx.*` | `ErrReservedName`, Open refuses |
| `xtx.format` min_reader newer than binary | `ErrFormatTooNew` / `ErrXTxUnsupported` |

`SnapshotTo` (`engine/snapshot.go:36,76`) is mutually exclusive with in-flight XTx
(`xtxSnapshotMu`) and copies `xtx.format` + `xtx.journal` so a restore is all-or-nothing in every
participant. `Verify` has `xtx-*` findings; `Repair` refuses to fabricate commits
(`engine/repair.go:276-284`, design §10 I6).

**Evidence.** `engine/xtx_recovery_test.go`, `engine/xtx_gate_test.go`, `engine/xtx_journal_test.go`,
`internal/xtxspec/recovery.go` (pure truth-table reference, frozen fixtures).

**Gaps.**
- **Gap R1 — no retirement/checkpointer.** `RETIRE`, journal GC and de-stamping are designed
  (design §4.4–§4.5) but **not implemented**: recovery explicitly leaves decisions live
  (`engine/xtx_recovery.go:383-387`) and compaction keeps committed stamped evidence verbatim
  (`engine/compactor.go:543-546`). Effects: `xtx.journal` grows monotonically; stamped entries are never
  converted back to plain v1 entries; `xtx.format`/journal can never be "drained" (§11.5 `xtx drain` not present).
  Warden must size for unbounded journal growth and must not promise a "drain back to legacy" operator path.
- **Gap R2 — no recovery observability** beyond the error returned from `Open` (§3.9).
- **Gap R3 — `xtx.format` records `created_by: "v1.4.0"` hardcoded** (`engine/db.go:313`); not reliable provenance.
- **Gap R4 — `Repair` refuses XTx roots with undecided/missing evidence** (returns `ErrXTxIncomplete`):
  there is no "presume abort" operator path (`repair --xtx-presume-abort` is designed, not shipped).
  Warden `repair` for `warden-db` must report and stop, not attempt auto-repair of XTx roots.

### 3.7 Nested behavior — **VERIFIED-IN-CODE (by absence)**

There is **no nested, savepoint or sub-transaction API.** A `CommitXTx` call is one flat, closed unit.
Consequences:
- A Warden operation that composes several store-level mutations must flatten them into **one** op list
  before the call. Two store methods that each call `CommitXTx` are two independent transactions.
- `CommitXTx` takes `c.mu` of every participant and blocks until it has them; a Warden-side lock ordering
  that waits on `CommitXTx` while another goroutine holds a participant lock and waits on Warden's lock
  can deadlock. Watch events are emitted after S6 with engine locks released, so calling `CommitXTx` from a
  watch callback does not self-deadlock on engine locks (not separately tested upstream).
- Same `(collection,id)` twice in one tx is rejected, so a "stage, then amend" pattern must be merged by Warden.
- A single-collection `Collection.Insert/Update/Delete` issued concurrently is serialized by the same `c.mu`
  but is **not** part of any XTx; mixing single writes and XTx for the same invariant is unsafe.
- The server's older single-collection `TxManager`/`CommitTx` (`engine/txmanager.go`, `engine/collection.go:1456`)
  is a different, per-collection mechanism; Warden's embedded path must not use it for multi-collection work.

### 3.8 Limits — **VERIFIED-IN-CODE** (hardcoded; not configurable)

| Limit | Value | Source | Notes |
|---|---|---|---|
| ops per tx | 1000 | `defaultXTxLimits`, `engine/xtx_journal.go:75-77` | `ErrXTxTooLarge` |
| participants | 16 | same | `ErrXTxTooLarge` |
| bytes per tx | 64 MiB | same | **estimated** as `128 + len(collection)` (+256 if `Data != nil`) per op (`engine/xtx_commit.go:75-79`), not real encoded size |
| idempotency key | 128 B | `xtxMaxKeyLen`, `engine/xtx_journal.go:50` | `ErrXTxTooLarge` |
| txid | 33 chars | `xtxMaxTxIDLen`/format | `<16hex>-<16hex>` |
| journal record | 16 MiB | `xtxMaxJournalRecord` | |
| per-entry | collection `maxScanTokenSize` (16 MiB) | existing | oversized record → append error → `ErrXTxDurability` |
| status retention | **not implemented** | design §8.2 (24 h) | see Gap L2 |

**Gaps.**
- **Gap L1 — limits are not configurable** at the engine API (`defaultXTxLimits()` is called unconditionally,
  `engine/xtx_commit.go:82`); `xtxLimits.validate(replicationRing)` exists but is unused by commit.
  Warden must pre-split its own large cascades (agent delete cascade over many mailbox rows,
  project delete) into ≤1000-op chunks **and** accept that the chunks are not jointly atomic; design cascades
  as idempotent, resumable steps recorded by the operation journal.
- **Gap L2 — `EXPIRED` status is never produced** (no retention GC, see R1); `XTxStatus` for an old key
  stays `COMMITTED` forever (journal never GC'd).
- **Gap L3 — approximate byte accounting** means a tx of few, very large records can pass the 64 MiB limit and be
  rejected later only by quota/segment limits.

### 3.9 Observability — **UNKNOWN/GAP**

What exists:
- `engine.XTxStatus(txOrKey)` (`engine/db.go:322`).
- `*XTxError` carries `Tx` (txid) and wrapped cause; `errors.Is`/`As` work.
- Watch events for committed ops are emitted after S6 in participant-then-op order; aborted tx emit none
  (`TestCommitXTx_WatchEventsEmitted`).
- Existing per-collection hooks apply to stamped appends: `CollectionConfig.OnAppend(collection, bytes, err)`,
  `OnSegmentPoisoned(collection, segment, cause)` (`engine/collection.go:189,193`; `engine/segment.go:190,239`),
  `OnIntegrity`, `OnIndexRecovery`.
- `Verify`/`VerifyDir` findings with `xtx-*` codes (`engine/xtx_verify.go`).

What does **not** exist (design §14.12 asks for it, code does not have it):
- No XTx-specific metric hook (commit count/latency, abort reason, conflict count, outcome-unknown count,
  recovery duration/aborted-runs count, journal size/record count, in-flight count).
- No programmatic recovery report returned by `Open` (counts of presumed-aborted tx). Only errors.
- No journal introspection API (list pending/undecided, last seq, size).

**Warden must** measure around the call (duration, error class, participant set, op count, txid from
`XTxResult.TxID` or `XTxError.Tx`) in its own transaction wrapper and expose those through its existing
telemetry; it must log the txid on every outcome-unknown. Do not wait on upstream hooks for the first release.

## 4. Gaps and risks that `transaction-boundary` must design around

Ordered by consequence for the Warden schema-2 contract.

| # | Gap / risk | Required Warden-side design response |
|---|---|---|
| G1 | **No transactional reads / no FK or invariant engine** (I1, I3, I4). Collection contract §6.1 "engine rejects violations" cannot be delegated. | Boundary exposes only *typed, pre-validated* write sets. Validation reads happen under a Warden-level per-domain writer lock (or are fenced with `ExpectedRev` touch-ops) immediately before `CommitXTx`; FK/restriction checks live in the owner packages, never silently skipped. |
| G2 | **No nested / savepoint / staging API** (§3.7). | Narrow abstraction: `Begin` accumulates an in-memory op list per owner, `Commit` makes exactly one `CommitXTx` call; `Rollback` discards. Reject re-entrancy and nested `Begin` with a typed error. No implicit long-running transactions: a tx object has no open engine resource, so the "no long-running tx" rule is a deadline on *Warden's* lock + a max-staging-age guard. |
| G3 | **Existing-id insert not rejected** (A1). | Ids only from `ReserveID`; `Insert` ops go through a unique key index; the boundary must refuse an insert when the caller cannot prove the id is fresh. |
| G4 | **`ErrXTxOutcomeUnknown` is the only ambiguous result** and is not retry-safe (§2.4). | Every Warden commit supplies an idempotency key; on outcome-unknown the boundary resolves via `XTxStatus(key)`; `UNKNOWN` ⇒ retry as a new tx, `COMMITTED` ⇒ success, `PENDING` ⇒ wait. Never return success on outcome-unknown without a status confirmation. Replay `Rev` is untrustworthy (C3): re-read after resolving. |
| G5 | **Idempotent replay ignores payload** (C3). | Key = `<operation-journal-id>/<step>/<payload-hash>`; boundary never reuses a key for different ops. |
| G6 | **Errors are sentinel+string** (C1) and several are untyped (§2.4). | One classifier (`Conflict`, `Exhausted`, `TooLarge`, `Durability(restart)`, `Unknown(outcome)`, `NotFound(collection)`, `Internal`) over `errors.Is`; default to fail-closed/non-retryable. Retry only `Conflict`/`Exhausted`/`Durability` per `XTxRetrySafe`, bounded, with a fresh read before each retry. |
| G7 | **Unbounded lock wait; no ctx/timeout** (C2). | Run each commit in its own goroutine with a Warden deadline; on deadline the boundary reports `Unknown` *only after* resolving via key status, because the goroutine may still commit. Never hold Warden locks across the call except the per-domain writer lock, whose holder must tolerate the wait. |
| G8 | **Collections and indexes are not transactional** (§3.5). | Bootstrap/migration creates all schema-2 collections and indexes first; boundary rejects any op against an uncreated collection (`ErrCollectionNotFound` surfaced as a startup error, not a runtime retry). |
| G9 | **Limits fixed at 1000 ops/16 participants/64 MiB, not configurable** (L1). | Boundary enforces the same numbers *before* calling (fail early, typed). Large cascades are decomposed into resumable steps driven by the operation journal, each atomic, not jointly atomic. |
| G10 | **No retirement/GC: journal grows, format is one-way** (R1, §1 downgrade hazard). | Document as accepted for schema-2 v1. `wd rollback`/backup path must restore a pre-first-XTx data dir. Disk-growth check in `wd doctor`/store health: report `xtx.journal` size. |
| G11 | **Fixed durability cost (n+1 fsync), no journal group commit** (D1). | Reserve XTx for the contract §6.3 atomic list. High-rate streams (usage, spend, metrics, events) stay single-collection. |
| G12 | **No XTx metrics/recovery report** (§3.9). | Boundary emits its own counters/histogram and logs txid; `store_health` surfaces last-open recovery error class and journal size. |
| G13 | **Unreleased pseudo-version; API may change** (§1). Upstream API + isolation work is in flight. | Boundary is the **only** package allowed to import `engine.CommitXTx`/`XTxStatus`/`XTxOp` (enforce with an import test). Re-pin to the tagged release before shipping; re-run the capability tests listed in §6. |
| G14 | **`Repair` of XTx roots is a refuse-and-report** (R4). | Warden repair must treat `ErrXTxIncomplete`/journal errors as reportable integrity findings requiring operator action; no silent fix. |

## 5. Hard constraints (carried into every downstream task)

1. **Atomicity is never emulated.** If `CommitXTx` is unavailable, returns `ErrXTxUnsupported`, or the
   boundary cannot prove the transaction fits the limits, the operation fails with a typed error. The
   boundary must not fall back to sequential single-collection writes, "best effort then compensate", or
   accepting a partial write set. A multi-collection invariant that cannot be done atomically is rejected,
   not degraded.
2. **Transactions cover internal DB state only.** tmux sessions, git worktrees/branches, transcript blobs,
   plan YAML export, hook staging files, hub/network calls are external side effects governed by the
   operation journal (`2026-10-10-warden-db-operation-journal-and-reconciliation.md`). The documented pattern
   is *record the intent in the DB transaction first, perform the effect, then record the outcome in a second
   transaction*; an external effect is never performed inside or as a condition of a XTx commit.
3. **Single process, single data dir.** Warden's embedded DB is that scope. Any future multi-process or hub
   topology must not rely on XTx for cross-process atomicity.

## 6. Verification performed and re-run checklist

Performed against `/home/srjn45/dev/scriva` @ `74123d0` (commit-equivalent to `4ca8d00` for engine code):
code read of `engine/xtx_commit.go`, `xtx_errors.go`, `xtx_journal.go` (limits/format/status), `xtx_recovery.go`,
`db.go`, `snapshot.go`, `compactor.go`; design docs `design-cross-collection-transactions.md`,
`design-xtx-fault-boundaries.md`; `go test ./engine -run XTx` passes.

To re-run at re-pin time (no wrapper code needed):
1. `go test ./engine -run 'XTx|Xtx' -race` in the upstream checkout at the target tag.
2. Diff `engine/xtx_commit.go`, `engine/xtx_errors.go` exported symbols against §2; any removed/renamed
   symbol blocks the re-pin.
3. Re-confirm each Gap above: A1 (existing-id insert), C3 (replay `Rev`), R1 (retirement), L1 (limits
   configurable), and whether a façade/isolation API was added upstream.
4. `go build ./... && wd check` after `go get github.com/srjn45/scriva@<tag>`; regenerate
   `THIRD-PARTY-NOTICES.md` with `scripts/gen-notices.sh`.
