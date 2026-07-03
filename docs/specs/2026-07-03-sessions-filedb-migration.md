# Migrating `internal/store` (session storage) onto embedded FileDB

**Date:** 2026-07-03
**Status:** Design (not yet implemented — docs-only)
**Owner:** Srajan Pathak
**Branch / worktree:** `sessions-filedb-design` (`.worktrees/sessions-filedb-design`)
**Scope:** `internal/store` (`file.go`, `store.go`, `portability.go`,
`customtype.go`), `internal/cli/daemon.go` (wiring only). No daemon API change —
the `Store` interface is preserved verbatim, so `internal/daemon`,
`internal/client`, `internal/mcp` are untouched.

---

## 1. Problem & context

`internal/store` persists every agent session as **one pretty-printed JSON file**
per record: active sessions under `<DataDir>/sessions/<id>.json`, archived ones
under `<DataDir>/closed/<id>.json` (`internal/store/file.go`). Every mutation —
even a single-field status flip or one appended event — rewrites the whole file
via temp-file + rename. This is the same write-amplification pattern that
`internal/ctxstore` and `internal/mailbox` carried before commit `ed5d50b9`
("feat(ctxstore,mailbox): back the two write-amplification stores with embedded
FileDBv2"), which moved those two "offenders" onto an embedded
`github.com/srjn45/filedbv2` collection each (`SyncModeNone`), so a write appends
one record instead of rewriting a whole file.

That commit **intentionally left the session store on `FileStore`**. This spec
designs its migration onto the same embedded FileDB, mirroring
`ctxstore.Store`/`mailbox.Store`, with one property those two did not have:

> **Sessions are durable user history.** ctxstore and mailbox are ephemeral
> coordination scratch space — losing them on upgrade is a shrug. Losing a
> user's session history on upgrade is a bug. So this migration must carry a
> **one-time import** of the existing `sessions/` + `closed/` JSON files into the
> new collections, and a **rollback story** if that import fails partway.

### What FileDB gives us (recap from the engine, `filedbv2@v0.2.1`)

- `filedb.Open(dir, WithSyncMode(SyncModeNone))` → a `*filedb.DB` rooted at
  `dir`; `db.Collection(name)` opens/creates a **sub-directory** collection.
  `engine.Open` pre-opens **every sub-directory of the root as a collection** and
  **skips plain files** (`engine/db.go`). This is the single most important
  layout constraint — see §3.
- Records are keyed by a caller-supplied string stored in the reserved
  `engine.KeyField` (`"_key"`). `InsertWithKey` / `GetByKey` / `UpdateByKey` /
  `DeleteByKey` / `Upsert` operate by key; `Scan(query.MatchAll)` walks the
  collection; `EnsureIndex` / `IndexLookup` give O(matches) secondary lookups;
  `UpdateIfMatch` / `UpdateIfRev` are the lock-free CAS primitives ctxstore uses.
- `LoadJSONL(r, keyField)` bulk-loads NDJSON **atomically** (all-or-nothing;
  parse+validate the whole batch, then one write-locked `CommitTx`). A malformed
  line or a duplicate key aborts the load with **nothing written**. This is the
  backbone of the import (§5) and its rollback (§6).
- `Close()` flushes the index and stops the background compaction goroutine — so
  the store now genuinely needs `Close()` (today's `FileStore.Close` is a no-op).

---

## 2. Goals / non-goals

**Goals**

1. Replace per-file JSON writes with an embedded FileDB, mirroring the
   ctxstore/mailbox port (`SyncModeNone`, offenders-first).
2. Keep the `store.Store` interface **byte-for-byte identical** so nothing
   downstream (daemon/client/mcp/portability) changes.
3. Import **all** existing `sessions/` + `closed/` JSON on first open, losing no
   history, subsuming `file.go`'s `migrateProvenance()` backfill.
4. Fully-recoverable rollback if the import dies partway.

**Non-goals**

- No `Store`-interface additions (no new query methods, no field indexing exposed
  to callers). Field-level indexes are an *internal optimization*, opt-in later.
- No change to `Session` shape, to `warden export`/`import`, or to the daemon API.
- No deletion of the legacy JSON on this release — it is kept as a cold backup
  (see §6). A later release removes it once the FileDB path is proven.

---

## 3. Collection layout — **two collections (`active` + `closed`)**

**Decision: two collections keyed by session id — an `active` collection and a
`closed` collection — NOT one collection with a status/archived discriminator
field.** This is the primary handoff decision.

### On-disk root — a NEW dedicated directory

The FileDB **must not** be rooted at the existing `<DataDir>/sessions/` directory.
`engine.Open` opens every sub-directory of its root as a collection; rooting there
is asking it to interpret leftover artifacts as collections, and (though it skips
the legacy `*.json` *files*) it muddies a directory whose semantics we are
retiring. Mirror the ctxstore/mailbox precedent, which each get their own root
(`<DataDir>/context/`, `<DataDir>/inbox/`):

```
<DataDir>/
  sessions/            # legacy JSON, active   — READ-ONLY source for import, kept as backup
  closed/              # legacy JSON, archived — READ-ONLY source for import, kept as backup
  sessions-db/         # NEW — filedb.Open root for the session store
    active/            #   db.Collection("active")  — one live session per record, _key = id
    closed/            #   db.Collection("closed")  — one archived session per record, _key = id
  .sessions-filedb-imported   # NEW sentinel — one-time import completed (see §5/§6)
```

`NewFileStore(dir)` becomes: `filedb.Open(filepath.Join(dir, "sessions-db"),
WithSyncMode(SyncModeNone))`, then `db.Collection("active")` and
`db.Collection("closed")`.

### Why two collections, not one-with-a-flag

The `Store` interface draws a hard line between **active** and **closed** that
today is *structural* — two directories — and every method leans on it:

- `Get`, and **all ~15 mutators** (`Update`, `UpdateStatus`, `AppendEvent`,
  `FinalizeExit`, …), read/write **only** `sessions/`. An archived session is
  invisible to `Get` (returns `ErrNotFound`) and immutable to mutators.
- `UpdateStatusIf` / `FinalizeExit` treat "missing from active" as
  `(false, nil)` — a CAS against an archived/deleted id is a no-op, not an error.
- Name-uniqueness (`Insert`, `GetByNameOrID`) is enforced **only among active**
  sessions; archived records keep their name and never collide.
- `List` scans active; `ListClosed` scans closed.

| | **Two collections** (chosen) | **One collection + `archived` flag** |
|---|---|---|
| "active-only" ops | **structural** — method targets the `active` collection | must add `archived=false` filter to Get + every mutator; forget one and you mutate a closed record |
| `List` / `ListClosed` | `Scan` each collection | needs a secondary index on `archived` + filtered scans |
| `Archive` | read active → `InsertWithKey` closed → `DeleteByKey` active (2 writes) | single `UpdateByKey` flag flip (1 atomic write) ✔ |
| Crash mid-`Archive` | at-worst-in-both, recoverable (matches today exactly) | no dual-appearance window ✔ |
| Semantic risk | **low** — 1:1 with today's directory split | medium — discriminator threaded through many methods |

The single-collection design's *only* real win is a cleaner `Archive` (one atomic
flip vs. a two-write move). It loses on everything else: it converts an invariant
that is free-and-structural today ("closed sessions can't be seen or mutated")
into a filter that must be *remembered* in Get and every mutator. That is exactly
the class of bug we don't want in durable user history. mailbox's
single-collection-with-secondary-index shape fits a *fan-out* access pattern
(messages per recipient); sessions are a **partition** (a record is active XOR
closed), which two collections model directly.

`Archive`'s two-write move keeps today's crash-recovery contract verbatim (from
`file.go`): write the closed copy **first**, delete the active record **second**,
so a crash between them leaves the session recoverable in `active`, never lost —
at worst it appears in both, and `Get`/`List` (active) still find it. Re-archiving
an id already in `closed` overwrites via `Upsert` on the closed collection,
matching "an existing `closed/<id>.json` is overwritten."

---

## 4. Record-key strategy — `KeyField` = session id, body = the whole `Session`

**Decision: `engine.KeyField` (`_key`) = `Session.ID`** — the same string used
today as the `<id>.json` filename — exactly mirroring ctxstore's "each key is a
record keyed by the context key." One session ⇒ one keyed record in its
collection.

**Body: the whole `Session`, decomposed into the record map via a JSON
round-trip.** Not a single opaque JSON blob field.

```go
// write
func toRecord(s *Session) (map[string]any, error) {
    b, err := json.Marshal(s)          // Session's own json tags — lossless
    if err != nil { return nil, err }
    var m map[string]any
    if err := json.Unmarshal(b, &m); err != nil { return nil, err }
    return m, nil                       // engine stamps _key on InsertWithKey/Upsert
}

// read
func fromRecord(d map[string]any) (*Session, error) {
    b, err := json.Marshal(d)          // includes the extra "_key" field…
    if err != nil { return nil, err }
    var s Session
    return &s, json.Unmarshal(b, &s)    // …which Session has no tag for → dropped
}
```

Rationale and rules for the implementer:

- **Always round-trip through JSON; never read typed business logic off the raw
  map.** FileDB records are JSON on disk, so a `map[string]any` returns numbers as
  `float64`, times as strings, etc. Marshaling the `Session` and unmarshaling the
  map back with the *same* json tags is lossless (it is the same byte stream
  modulo key order): `PID int`→`float64`→JSON number→`int`, `*int`/`*time.Time`
  pointers and `omitempty` all survive. Reading a field like `PID` directly off
  the map for logic would reintroduce the float64 hazard — don't.
- The reserved `_key` field the engine stamps into `Data` is harmlessly ignored
  on read: `Session` has no `_key` json tag, so `json.Unmarshal` drops it.
- **Decomposed (map) over single-blob**: a `{"_key":id,"doc":"<json>"}` blob is
  simpler to round-trip but opaque to FileDB — it forecloses ever secondary-
  indexing `name`, `pipeline_id`, or `status`. The decomposed body keeps those
  fields real, consistent with how ctxstore/mailbox decompose. We don't index any
  today (see below), but we keep the door open at zero cost.
- **Name-uniqueness stays an O(n) active scan** (as today's `listLocked` loop),
  not a secondary index — session counts are small and this is the faithful port.
  A `db.Collection("active", WithUniqueIndex("name"))` unique index is the natural
  *future* optimization (it would also enforce uniqueness structurally), noted but
  **out of scope** to keep the port minimal and behavior-identical. (Caveat if
  adopted later: `name` is `omitempty` and empty for no-name agents; a unique
  index over many empty-string values must be avoided — index only non-empty, or
  keep the scan.)

### Validation gates are unchanged

`safeID` (rejects `/ \ :` and `..`), `safeSessionRef`, and `ValidateName` run
**before** any collection write, exactly as in `file.go`. Session ids are far more
restrictive than FileDB keys require, so nothing new is needed. These gates also
protect the import: a legacy file whose decoded id fails `safeID` is handled per
§5.

---

## 5. Legacy-JSON import on open (subsumes `migrateProvenance`)

`NewFileStore(dir)` runs a **one-time import**, guarded by a sentinel file, right
after opening the collections and before returning the store (i.e. before the
daemon serves any request — no lock needed, same as today's `migrateProvenance`).

```
NewFileStore(dir):
  db  := filedb.Open(dir/sessions-db, SyncModeNone)
  active, closed := db.Collection("active"), db.Collection("closed")
  if sentinel .sessions-filedb-imported exists:            // already imported
     return store
  importLegacy(dir, active, closed)                        // §5.1
  write sentinel  .sessions-filedb-imported (RFC3339 + "\n")
  return store
```

### 5.1 `importLegacy` — fold provenance backfill into a single pass

The old `migrateProvenance()` walked the same legacy files to backfill
`WorktreeCreated`/`BranchCreated` on pre-feature records, guarded by its own
`.provenance-migrated` marker. Rather than run it as a separate pass that rewrites
files we're about to abandon, **fold that backfill into the importer**:

```
importLegacy(dir, active, closed):
  provDone := exists(dir/.provenance-migrated)   // did old code already backfill?
  for (srcDir, col) in [(dir/sessions, active), (dir/closed, closed)]:
     var buf bytes.Buffer                         // NDJSON, one session per line
     for each *.json in srcDir (skip .tmp-*, skip subdirs):
        s, err := readSession(file)
        if err != nil:                            // unreadable/corrupt
           slog.Warn("skipping unreadable legacy session", file, err); continue
        if err := safeID(s.ID); err != nil:
           slog.Warn("skipping legacy session with unsafe id", file); continue
        if !provDone:                             // record predates provenance feature
           backfillProvenance(s)                  // same rule as today
        writeJSONLine(&buf, s)                    // marshal s → one NDJSON line
     col.LoadJSONL(&buf, "id")                    // atomic per collection; keyField="id"
```

Notes:

- **`keyField="id"`**: `Session` already serializes an `"id"` field;
  `LoadJSONL(r, "id")` stamps `_key` from it. So the record key is the session id,
  matching §4 with no extra plumbing.
- **Corrupt-file handling matches today.** `file.go`'s `listDir`/`migrateProvenance`
  *skip* an unreadable session with a warning (a corrupt file is already invisible
  in production). The importer decodes each file **individually** and skips+warns
  on parse error, so one bad file never blocks the upgrade. Only the good, decoded
  records reach `LoadJSONL`, so the batch it sees is always parseable — its
  all-or-nothing guarantee then protects against a *write*-side failure, not a
  single poison file. (If we fed raw file bytes to `LoadJSONL`, one corrupt file
  would abort the entire import — the wrong trade for "lose no history.")
- **Provenance semantics preserved exactly.** If `.provenance-migrated` exists,
  the old code already wrote explicit flags into the legacy JSON — we import them
  verbatim (`provDone=true`, no re-backfill), so a legitimately-adopted
  (`WorktreeCreated=false`) record is never clobbered. If it's absent, we backfill
  during import — the same inference `migrateProvenance` would have made.
- **`migrateProvenance()` and its `.provenance-migrated` marker are deleted**
  from `file.go`; its behavior lives on inside `importLegacy`. The old marker is
  now read-only input (did the old code already backfill?) and can be left on disk
  harmlessly.
- **`backfillProvenance` is retained** (moved alongside the importer) — its
  inference rule (`WorktreeCreated = Worktree != ""`; `BranchCreated = Branch ==
  ID`) is unchanged.

### 5.2 Idempotency of a completed import

Once the sentinel exists, `NewFileStore` never re-imports. New sessions written
post-upgrade go **only** to the FileDB collections; the legacy `sessions/`/`closed/`
dirs are frozen (never written, never deleted by this release).

---

## 6. Rollback if import fails partway

The design makes the import **atomic at the directory level** and keeps the legacy
files as the durable ground truth, so any partial failure is fully recoverable.

### Invariants that make rollback safe

1. **Legacy `sessions/` and `closed/` are read-only to the importer and are never
   deleted by this release.** They remain a complete, untouched copy of all
   pre-upgrade history — the ultimate backup.
2. **`sessions-db/` is a *derived artifact* until the sentinel is written.** The
   sentinel is written **last**, only after *both* `LoadJSONL` calls succeed.

### The failure ladder

- **Fail during decode** (corrupt file): skipped+warned, import continues. No
  failure — a corrupt session was already invisible pre-migration.
- **Fail during a `LoadJSONL` write** (disk full, I/O error): that collection's
  load is atomic, so it is **fully applied or fully untouched**. But the *other*
  collection may already be loaded (e.g. `active` loaded, then `closed` fails).
  The sentinel is **not** written, so `NewFileStore` returns the error and the
  daemon refuses to start.
- **Recovery on next start:** because the sentinel is absent, we must re-import —
  but re-running `LoadJSONL` over an already-populated collection would hit
  `ErrDuplicateKey` and abort. So, **before importing, if the sentinel is absent,
  delete any pre-existing `sessions-db/` directory and recreate it empty**,
  guaranteeing a clean slate:

  ```
  NewFileStore(dir):
     if !exists(dir/.sessions-filedb-imported):
        os.RemoveAll(dir/sessions-db)      // wipe any partial/failed prior attempt
     db := filedb.Open(dir/sessions-db, SyncModeNone)
     ... open collections ...
     if !exists(sentinel):
        importLegacy(...); write sentinel
  ```

  This makes the whole import **all-or-nothing across both collections**: either
  you end with a fully-imported `sessions-db/` + sentinel, or the next boot wipes
  the half-built db and retries from the intact legacy files. Wiping is safe
  precisely because `sessions-db/` holds no data that isn't reproducible from
  `sessions/`+`closed/` until the sentinel says the import finished.

### Operator-level rollback (downgrade the binary)

Because the legacy JSON is left intact, an operator can downgrade to a pre-
migration binary and it will read `sessions/`+`closed/` as before — **no
pre-upgrade history is lost**. The one-way caveat, which must be **documented in
the release notes**: sessions *created or mutated after* the upgrade live only in
`sessions-db/`, so a downgrade cannot see them (and mutations to pre-upgrade
sessions made post-upgrade are likewise only in the FileDB). This is inherent to
any forward migration and is acceptable; the guarantee we make is "no pre-upgrade
history is ever lost," not "upgrade is bidirectionally transparent."

A later release, once the FileDB path has soaked, can remove the legacy dirs
(guard behind its own sentinel / an explicit `warden maintenance` step) — out of
scope here.

---

## 7. Interaction with `portability.go` and `customtype.go`

### `portability.go` (`Export` / `ImportResult`, `warden export`/`import`)

These are **transport DTOs only**; the exporter/importer operate entirely through
the `Store` interface (`internal/daemon/import_routes.go` calls `Get` → `Delete`
→ `Insert`). Because the migration preserves that interface's semantics verbatim,
`warden export`/`import` keep working with **no change**:

- `Insert` still returns `ErrExists` (id already in `active`) and `ErrNameExists`
  (name collides with a *different* active session); the importer's `--merge`
  path (`Delete` then `Insert`) and its name-drop-and-retry path both depend only
  on those errors — unaffected.
- **Preserve one pre-existing quirk:** `importSessions` calls `st.Get` to test
  existence, and `Get` consults **active only**. So importing a record whose id
  exists solely in `closed` is *not* detected as existing → it lands in `active`,
  and the id then exists in both collections. This is already true today (`Get`
  reads `sessions/` only), so the two-collection design reproduces it exactly —
  **not a regression, do not "fix" it as part of the migration.**
- `Export`/`ImportVersion` envelope shape is untouched (still metadata-only; no
  worktree/branch files serialized).

### `customtype.go` (plugin-registered `Type` values)

Pure in-memory enum logic (`Type.Valid`/`NormalizeType`/`DefaultWorktree`
consulting the `customTypeLookup` seam) — **storage-agnostic**. The only storage
touch-point: a `Session.Type` is a plain string in the record body. The JSON
round-trip (§4) preserves it **verbatim**, including a plugin-registered custom
type, independent of whether that plugin is loaded at read time — identical to
today's JSON files. (`NormalizeType` still collapses a genuinely-unknown string to
`"other"` only when something re-normalizes it; storage never does.) No change.

---

## 8. Concurrency, `Ping`, `Close` (implementer checklist)

- **Serialize compound read-modify-write with a store `sync.Mutex`**, mirroring
  mailbox's choice (not `FileStore`'s `RWMutex`, since FileDB does its own
  per-collection locking). The methods that read-then-write and therefore need the
  store lock held across both halves: `Insert` (name-uniqueness scan **then**
  `InsertWithKey` — must be one critical section, as today), `Update`/`mutate` and
  every setter funneling through it, `UpdateStatusIf`, `FinalizeExit`, and
  `Archive` (read active → write closed → delete active). Read-only methods
  (`Get`, `List`, `ListClosed`, `GetByNameOrID`) can run under the collection's
  own locking; taking the store mutex (read side, if you keep an `RWMutex`) is the
  conservative faithful port. *Alternative considered:* implement the CAS methods
  with `UpdateIfMatch`/`UpdateIfRev` (as ctxstore does) and drop the mutex; viable
  but changes the reasoning model — recommend the mutex for a behavior-identical
  first cut, optimize later.
- **Multi-field atomic writes get *better*.** `FinalizeExit` (status + exit code +
  appended event) and `AppendEventStatus` become a single `UpdateByKey` of the
  whole record — atomic at the storage layer, strictly stronger than today's
  temp-file rewrite.
- **`Ping`**: replace the `sessions/` dir-stat with a cheap liveness check on the
  db (e.g. `active.Count(query.MatchAll)` erroring, or stat `sessions-db/`).
- **`Close(ctx)` must now call `db.Close()`** to flush the index and stop the
  compaction goroutine — today it's a no-op. `daemon.go` already does
  `defer st.Close(context.Background())`, so no wiring change is needed there
  beyond making `Close` real. (ctxstore/mailbox set this precedent.)
- **`SyncModeNone`** is retained deliberately: like the previous implementation
  this is a localhost session store, so the last write surviving a power-loss is
  not a requirement; append-only segments rule out torn reads regardless. This
  matches the ctxstore/mailbox rationale verbatim.

---

## 9. Test plan (for the implementation stage)

- **Import fidelity:** seed a `DataDir` with legacy `sessions/`+`closed/` JSON
  (including one no-name agent, one tagged, one with `ExitCode`/rate-limit
  pointers, one archived), open `NewFileStore`, assert `List`/`ListClosed`/`Get`
  return byte-identical `Session` values (round-trip equality).
- **Provenance fold:** (a) legacy tree *with* `.provenance-migrated` present +
  explicit flags → imported verbatim (adopted record keeps `WorktreeCreated=false`);
  (b) *without* the marker → `backfillProvenance` applied. Port the existing
  `provenance_test.go` cases onto the new path.
- **Corrupt legacy file:** a garbage `.json` is skipped+warned; all other sessions
  still import; the run still writes the sentinel.
- **Rollback:** simulate a `LoadJSONL` failure on the `closed` collection (e.g.
  inject an error / fill the batch with a dup) → `NewFileStore` errors, sentinel
  absent; re-open succeeds and wipes+reimports to a correct state.
- **Idempotent re-open:** second `NewFileStore` on an imported tree does not
  re-import and does not duplicate records.
- **Interface parity:** run the existing `file_test.go` suite against the new store
  unchanged (it exercises the whole `Store` interface) — the strongest guarantee
  the behavior is identical.
- **`warden export`/`import` E2E** (`import_routes_test.go`): default-skip,
  `--merge` overwrite, and name-collision rename paths, plus the "id only in
  closed" quirk (§7) asserted to still land in active.

---

## 10. Handoff — decisions the next stage implements directly

1. **Layout: two collections** `active` + `closed`, each keyed by
   `engine.KeyField = Session.ID`, rooted at a **new** `<DataDir>/sessions-db/`
   (never at the legacy `<DataDir>/sessions/`). Body = the whole `Session`
   decomposed into the record map via a JSON round-trip; read back the same way
   (the stamped `_key` is dropped on unmarshal). Name-uniqueness stays an O(n)
   active scan; a `name` unique index is a documented future optimization only.
2. **Import path: one-time, sentinel-guarded, fold-in provenance.**
   `NewFileStore` → if `.sessions-filedb-imported` absent: `RemoveAll(sessions-db)`,
   open collections, `importLegacy` (decode each legacy file individually,
   skip+warn on corrupt, `backfillProvenance` iff the old `.provenance-migrated`
   marker is absent, `LoadJSONL(buf, "id")` per collection), then write the
   sentinel **last**. Delete `migrateProvenance()`; keep `backfillProvenance`.
   Legacy dirs are read-only and kept as backup; `Close` now calls `db.Close()`.
   Rollback is directory-atomic: no sentinel ⇒ wipe-and-retry from the intact
   legacy JSON; operator downgrade still reads the legacy JSON (pre-upgrade
   history never lost; post-upgrade writes are FileDB-only — document in release
   notes).
