# Release notes — Agent-store integrity and recovery (#795)

## Highlights

- The daemon is the single writer for `<data_dir>/agents-db`. It acquires a
  canonical-path, kernel-released ownership lock before import, migration,
  reconciliation, or opening the store, and holds it through `Close`.
- A competing daemon, including one on another port or using a relative,
  absolute, or symlinked path, fails before it can mutate the store. The error
  tells the operator to stop the existing daemon; deleting the lock file is not
  a recovery step.
- Reads now fail complete-or-error: a bad index result, key/record mismatch, or
  incomplete scan is reported as an unhealthy store instead of a partial fleet.
  Running tmux sessions are neither relaunched nor changed.
- `warden doctor`, `GET /api/v1/store/health`, MCP `store_health`, and the TUI
  expose the health state. The TUI retains its last complete snapshot while
  clearly showing that it is stale.
- Membership reconciliation acts only on verified identities and reports
  duplicate or ambiguous identities rather than selecting one.

## Upgrade guidance

No data-format migration is required. Stop the daemon cleanly, back up the
entire private data directory, then start one upgraded daemon for that directory.
Use `warden doctor` after startup. Offline verify/repair uses
`warden inspect repair agents` (`warden repair agents` is a compatibility alias)
and must run only after the daemon has stopped. This release depends on
**ScrivaDB v1.4.0** (never v1.2.0).

The supported locking assumption is a local filesystem with reliable POSIX
`flock` semantics. NFS, SMB, and FUSE filesystems without equivalent semantics
are unsupported for the agent-store data directory.

## Offline repair (ScrivaDB v1.4.0)

`warden inspect repair agents` now verifies and repairs the agent store through
ScrivaDB v1.4.0 (`repair_available: true` in store health). It is offline-only
and CLI-only, and refuses a daemon-owned store with a "stop the daemon" next step.

- `--dry-run` is read-only; `--json` emits the machine-readable report.
- A real repair first takes a SHA-256-verified backup
  (`<backup-dir>/repair-backup-<UTC>/`, default parent: the data dir; prefer
  `--backup-dir` outside it), then atomically rebuilds indexes from segments.
  Segments are never edited and tombstones stay deleted.
- It is journaled, restartable and idempotent: re-run the same command after an
  interruption; a repaired store is a no-op with no new backup.
- Conflicts (duplicate ids, conflicting revisions) are reported, never resolved
  (`--on-conflict report|abort`). `--salvage` is opt-in and quarantines damaged
  originals byte-for-byte instead of deleting them.
- Keep the backup, `repair-report.json`, `quarantine/` and JSON output as incident
  evidence. Attempts emit `audit: agent-store repair attempt` events.
- A damaged store makes the daemon start degraded on a scratch copy, leaving the
  on-disk store byte-identical; `/store/health` reports it and the SSE stream
  sends an explicit `event: error` (`degraded:true`) rather than going silent.
- Requires a local filesystem with reliable POSIX `flock`; ownership is a
  prerequisite, not a substitute for the backup.

The original incident did not prove what opened the second writer. A second
daemon and offline membership reconciliation remain candidate paths; this
release blocks those paths but does not claim a proven root cause; the original
second writer remains unproven. Other
ScrivaDB-backed stores are outside #795's ownership boundary and remain a
follow-up audit item.

See `site/src/content/docs/guides/agent-store-integrity.md` for the full
daemon-offline procedure and `docs/specs/2026-10-07-agent-store-integrity-validation-report.md`
for reproducible validation evidence.
