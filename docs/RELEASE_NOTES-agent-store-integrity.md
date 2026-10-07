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
Use `warden doctor` after startup. Offline checks use
`warden inspect repair agents` (`warden repair agents` is a compatibility alias)
and must run only after the daemon has stopped.

The supported locking assumption is a local filesystem with reliable POSIX
`flock` semantics. NFS, SMB, and FUSE filesystems without equivalent semantics
are unsupported for the agent-store data directory.

## Repair status and residual risk

This release does not implement an automated repair. The command above performs
only authority and ownership preflight checks, records an audit event, and makes
no mutation. Warden intentionally waits for ScrivaDB's upstream Verify/Repair
primitives (srjn45/scriva#107) rather than attempting an unsafe local rebuild.

The original incident did not prove what opened the second writer. A second
daemon and offline membership reconciliation remain candidate paths; this
release blocks those paths but does not claim a proven root cause. Other
ScrivaDB-backed stores are outside #795's ownership boundary and remain a
follow-up audit item.

See `site/src/content/docs/guides/agent-store-integrity.md` for the full
daemon-offline procedure and `docs/specs/2026-10-07-agent-store-integrity-validation-report.md`
for reproducible validation evidence.
