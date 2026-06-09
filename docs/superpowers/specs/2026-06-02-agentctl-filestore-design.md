# agentctl File-Based JSON Store (drop Mongo) — Design

**Date:** 2026-06-02
**Status:** Approved design (pre-implementation)
**Owner:** Srajan Pathak (personal project)
**Sub-project 1 of 5** in the "ship-friendly + restore + layered teardown + monitoring" direction (**1 storage** → 2 deterministic claude session-id → 4 restore → 3 layered teardown → 5 monitoring/notify).

---

## 1. Goal

Replace MongoDB with a **file-based JSON store** so agentctl ships with **no Docker/Mongo setup** — one JSON file per session, behind the existing `store.Store` interface, so nothing else in the codebase changes. Mongo is removed entirely.

## 2. Key decisions

| Decision | Choice |
|---|---|
| Backend | One JSON file per session; no server, no Docker. |
| Data dir | `AGENTCTL_DATA_DIR`, default `~/.agentctl`. Active: `~/.agentctl/sessions/<id>.json`; closed (archived): `~/.agentctl/closed/<id>.json`. |
| Concurrency | Disk-backed (no cache); a `sync.RWMutex` serializes ops (daemon handlers + poller + classify goroutine call it concurrently). |
| Atomicity | Write to a temp file in the same dir, then `os.Rename` (atomic on the same FS). |
| Mongo | **Removed** — delete `mongo.go`, `mongo_test.go`, `docker-compose.yml`, the `mongo-up`/`mongo-down` Makefile targets, and the mongo-driver/testcontainers deps. |
| Migration | None — switching backends starts empty (existing Mongo-tracked agents won't carry over). |

## 3. `FileStore` — implements the **full** `store.Store` interface

The interface currently has **14 methods** (note: `UpdateStatusIf` and `AppendEventStatus` were added by in-flight hardening WIP — the FileStore must implement them too):

`Insert, Get, List, UpdateStatus, UpdateStatusIf, UpdateType, UpdateSubject, AppendEvent, AppendEventStatus, UpdatePane, Archive, Delete, Ping, Close`

New file `internal/store/file.go`:

```go
type FileStore struct {
	mu       sync.RWMutex
	dir      string // data dir
	sessions string // <dir>/sessions
	closed   string // <dir>/closed
}
func NewFileStore(dir string) (*FileStore, error) // MkdirAll sessions/ + closed/
```

Behavior (all mutations under `mu.Lock()`, reads under `mu.RLock()`):
- **`Insert`** — `ErrExists` if `sessions/<id>.json` exists; stamp `CreatedAt` (if zero) + `UpdatedAt`; init `Events` to non-nil; atomic-write.
- **`Get`** — read+unmarshal `sessions/<id>.json`; `ErrNotFound` if missing.
- **`List`** — read every `sessions/*.json`, unmarshal, return sorted by `UpdatedAt` desc; **skip** (log) any file that fails to unmarshal (robustness).
- **`UpdateStatus`** — read-modify-write: set status, bump `updated_at`; `ErrNotFound` if missing.
- **`UpdateStatusIf(id, expected, next)`** — read; if `status == expected`, write `next` + bump, return `(true, nil)`; else `(false, nil)`. The `mu.Lock()` makes the read-check-write atomic (this is the whole point of the CAS).
- **`UpdateType` / `UpdateSubject` / `UpdatePane`** — read-modify-write the one field + bump.
- **`AppendEvent`** — append `ev` (stamp `ts` if zero) to `events[]` + bump.
- **`AppendEventStatus(id, ev, status)`** — append `ev` **and**, when `status != ""`, set status — single atomic write + bump.
- **`Archive`** — read `sessions/<id>.json`, atomic-write to `closed/<id>.json`, remove the active file; `ErrNotFound` if missing.
- **`Delete`** — remove `sessions/<id>.json`; `ErrNotFound` if missing.
- **`Ping`** — confirm the data dir is writable (used by `/healthz`).
- **`Close`** — no-op (no resources).
- **Safety:** reject `id` containing `/`, `\`, or `..` (returns an error) — ids are `agent-<hex>` / Jira keys, so this is defense, not a normal path.

Helper: `atomicWriteJSON(path string, v any) error` (CreateTemp in the dir → write → Rename).

## 4. Wiring & removals

- **`config`:** replace `MongoURI`/`DB` with `DataDir` (`AGENTCTL_DATA_DIR`, default `<home>/.agentctl`; fallback `.agentctl` if no home).
- **`cli/daemon.go`:** `st, err := store.NewFileStore(cfg.DataDir)` instead of `NewMongoStore(...)`. (Drop the `MongoURI`/`DB` references.)
- **Delete:** `internal/store/mongo.go`, `internal/store/mongo_test.go`, `docker-compose.yml`; remove `mongo-up`/`mongo-down` from the Makefile; `go mod tidy` to drop `go.mongodb.org/mongo-driver` and `testcontainers-go`.
- **Untouched:** the `store.Store` interface and all consumers (daemon/poller/lifecycle/mcp/cli) — they only see the interface. The daemon test `fakeStore` is unaffected.
- **README/Prerequisites:** remove Docker/Mongo from runtime requirements; document `AGENTCTL_DATA_DIR` (default `~/.agentctl`) and the on-disk layout. (`make mongo-up` references in docs removed.)

## 5. Error handling
- Missing file → `ErrNotFound`; existing on insert → `ErrExists` (unchanged contract).
- Corrupt/partial JSON in `List` → skip + log (one bad file doesn't break the listing). Atomic rename prevents partial files from normal operation.
- Unwritable data dir → `Ping` returns an error → `/healthz` 503 (matches Mongo-down behaviour).
- `id` with path separators → error (no path escape).

## 6. Testing (no Docker)
`internal/store/file_test.go` using `t.TempDir()` as the data dir — fast, hermetic:
- Insert+Get round-trip; `Insert` stamps `CreatedAt`; duplicate → `ErrExists`; `Get` missing → `ErrNotFound`.
- `UpdateStatus`/`UpdateType`/`UpdateSubject`/`UpdatePane` persist + bump `updated_at`.
- `UpdateStatusIf`: swaps when `expected` matches (→ true) and is a no-op when it doesn't (→ false), verified by re-Get.
- `AppendEvent` stamps `ts` + appends; `AppendEventStatus` appends **and** sets status atomically.
- `List` returns all, sorted by `updated_at` desc, and **skips a corrupt file** (write a junk `.json`, assert it's ignored).
- `Archive` moves to `closed/` (active `Get` → `ErrNotFound`); `Delete` removes (+ `ErrNotFound` on missing).
- `Ping` ok on a writable dir.
- id containing `/` → error.
- `-race` test: concurrent `UpdateStatus`/`AppendEvent`/`List` goroutines on one store, no races, no lost writes.

Other packages' tests are unchanged; the store package no longer needs a Mongo container (faster CI/local).

## 7. Dependency / sequencing note
The `FileStore` must implement the **current** interface including `UpdateStatusIf` + `AppendEventStatus`, which exist only in **uncommitted working-tree WIP** right now. Implementation must branch from a base where that WIP is committed (otherwise the FileStore is written against a stale interface and won't compile against the merged result). So: **commit the in-flight hardening WIP (and pause the agent producing more of it) before implementing this sub-project.**

## 8. Out of scope
- Mongo→file migration (none).
- The other four sub-projects (session-id, restore, teardown, monitoring).
- Encryption / multi-machine / remote storage.
