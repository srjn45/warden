# Agent-store integrity contract (issue #795)

Status: design record. Executable counterpart: `internal/agentstore/contract_test.go`
(skipped `TestContract*` tests are the acceptance gates for later tasks).

## 1. Open/write path audit (`<data>/agents-db`)

| Path | Where | Today | Required |
|---|---|---|---|
| Daemon bootstrap | `internal/cli/daemon.go` (`agentstore.New` before listen/reconcile/recovery/background jobs) | no ownership; second daemon (any port) touches data before port bind fails | own store first; ownership failure aborts before any mutation or goroutine; all stores closed on startup failure |
| Doctor reconcile | `internal/cli/doctor.go` `runMembershipReconcile` | opens writable with daemon live; comment claims `ErrStoreOwned` (false) | same ownership error; never opens a live-owned store |
| Legacy import | `agentstore.New` → `importSessions` (opens `sessions-db` via raw `scriva.Open`, and `os.RemoveAll(agents-db)` when marker absent) | wipe happens before any lock | lock acquired before RemoveAll/import/open; legacy dir read only |
| Migration markers | `.agents-from-sessions-imported`, `.archived-agents-from-sessions-imported` | written after import | unchanged; crash between import and marker must be retry-safe under the lock |
| Project membership | `daemon.ReconcileProjectMembership` (boot + doctor) | writes agents + projects | runs only under agent-store ownership |
| Factory reset | `internal/factoryreset/reset.go` | deletes dirs | must take ownership (or refuse) before deleting |
| Other scriva stores (pipelines, plans, projects, ctx, mailbox, schedule, snapshot, terminalstore, backendstore, autopilot*, knownprompts, planexport) | each `scriva.Open` | no lock | out of scope for fixes here; audit item for a follow-up: same overlapping-handle hazard |
| Tests | 12 test files call `agentstore.New` | share-dir reuse possible | tests must Close before reopen; helper seam: re-exec holder process |

Normal CLI/MCP/TUI operations go through the daemon; only the daemon and offline
tools (doctor, repair) may open the store.

## 2. Dependency matrix (ScrivaDB)

Pinned: `github.com/srjn45/scriva v1.2.1` (never v1.2.0). v1.2.1 exports **no**
exclusive-ownership, Verify, or Repair API (`scriva.DB`/`engine` have only
`Rebuild` on internal index types; `scriva.Open` takes no lock). Upstream issue
srjn45/scriva#107 tracks the overlapping-writer corruption.

Required of a future ScrivaDB release before the dependent warden tasks ship:
1. exclusive-open: `Open` fails with a typed error (sentinel) when another handle/process owns the dir; released on Close and process death.
2. `Verify`: validates every index entry against segment bytes (offset decodes to a record whose key matches), reports per-record problems, and makes normal scans return an error rather than silently skip bad locations.
3. `Repair`/rebuild from segments: offline, atomic, returns key/ID collisions as conflicts rather than choosing.

Warden must **not emulate** these (no local flock substitute inside agentstore is
a stopgap for the engine's lock: see §3 — warden-level process ownership is a
separate, required layer; index verify/repair must come from scriva). The
dependency test `TestContractScrivaDependencyPin` fails on any scriva bump so the
matrix is re-audited.

## 3. Ownership and lock compatibility

- Warden-level ownership: new lock file `<data>/.agents-store.lock`, flock-based
  (reuse the semantics of `internal/store/lock_unix.go`: kernel-released on death,
  file content diagnostic only). It is **distinct** from legacy
  `.sessions-store.lock`, which `store.FileStore` holds for `sessions-db`; that
  lock does not cover `agents-db` (pinned by
  `TestContractLegacyLockDoesNotCoverAgentStore`). The importer also reads
  `sessions-db`, so it must acquire the legacy lock read-side (or fail with
  `ErrStoreOwned`) before importing.
- Acquired before import/RemoveAll/open; held until `Close`.
- Error: `store.ErrStoreOwned` (reuse; wrapped with data dir and a hint to stop the
  daemon). Rejected opener must not mutate any file.
- Supported filesystems: local POSIX filesystems with working `flock` (ext4, xfs,
  APFS, btrfs, tmpfs). NFS/SMB/FUSE without flock semantics are unsupported;
  Windows uses the existing `lock_other.go` behavior. Different listen ports
  never bypass ownership because the lock is per data directory.

## 4. Read-degradation semantics

- Normal reads (`List`, `Get`, `ListClosed`, REST/SSE/CLI/TUI fleet views) are
  **complete-or-error**: if the engine detects (or Verify reports) an invalid
  index location the call returns a typed integrity error; never a nil-error short list.
- REST returns 500/503 with an actionable `agent store degraded: run warden repair agents`
  body; CLI exits non-zero; TUI keeps the last good snapshot and shows an explicit
  unhealthy indicator. `ListClosedDegraded` (skip-and-count) remains archive-only.
- Running sessions are unaffected: tmux sessions are never relaunched, adopted or
  killed by a read failure or by repair.

## 5. Repair conflict semantics

- Offline only (requires ownership); backup first into a persistent
  restrictive (0700/0600) location, originals preserved, atomic install, resumable
  after interruption, repeatable verify of lookup/list/membership agreement.
- Rebuild uses scriva's repair. Before trusting output, check logical-ID
  collisions (duplicate `id`, numeric-key vs logical-key mismatch). Colliding
  records are written to a conflicts report and left unselected; no record is
  silently chosen and no update may target an ambiguous identity.
- Identity preserved: agent IDs never regenerated. Membership: respect explicit
  removals/tombstones, tolerate intentional dangling membership, no re-adopt or
  duplicate relaunch of existing tmux agents; non-agent tmux sessions handled
  separately.

## 6. Open items
- Exact trigger of the production second writer is unidentified; candidates: `doctor --reconcile-membership`, a second daemon.
- Other stores' overlapping-handle exposure (§1).

## 7. Membership reconcile identity rules (p4)

`daemon.ReconcileProjectMembership` (boot + `doctor --reconcile-membership`) only
acts on verified identities. It reports, and never resolves, two conflict kinds:
`duplicate_agent_id` (one id in active+archived, or repeated within a set) and
`ambiguous_membership` (id claimed by several projects' `agents[]`). Conflicted ids
are skipped for restamping and legacy backfill; archived records are read-only.
Dangling members and explicit removals (non-nil forward lists) are preserved.
Reconcile edits only `project_id`/project lists — it never adopts, relaunches or
touches tmux sessions. Fixture: `internal/daemon/testdata/incident_membership.json`.

## 8. Entry-point wiring (p5)

| Surface | Behavior |
|---|---|
| Daemon startup | `*OwnershipError` aborts before any listener/import/goroutine; logs `audit: daemon startup refused` and prints the safe next step |
| `doctor` | "agent store" check: daemon-reported health when up; non-mutating ownership probe (`ProbeOwnership`) when down |
| REST | `GET /store/health` gains `repair_available` and `next_step`; 503 bodies carry `SafeNextStep` |
| MCP | read-only `store_health`; repair deliberately not exposed |
| TUI | degraded banner points to `warden doctor`; last-good snapshot retained |
| CLI `repair agents` | gated by `CheckRepairAuthority` (data-dir owner/root) + ownership probe; audits `denied`/`refused_owned`/`unavailable`; never mutates |

**Dependency (not implemented here):** the rebuild itself needs ScrivaDB
Verify/Repair (§2). `agentstore.RepairAvailable` is the single flag; flipping it
and replacing `RepairUnavailableError` is the follow-up once scriva ships them.
