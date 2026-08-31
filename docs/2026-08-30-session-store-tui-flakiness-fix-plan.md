# Session Store Ownership and TUI Fleet Stability — Implementation Plan

Date: 2026-08-30

## Context

The Cockpit TUI project pane intermittently loses agents and later shows them again. The agents and their tmux sessions remain alive; the visible fleet changes because the active session store can return incomplete snapshots after its append-only segment is corrupted.

The observed failure begins in the backend hot-swap path. `warden switch` opens a second writable `FileStore` directly while the daemon already owns the same ScrivaDB. `FileStore` uses an in-process mutex and assumes the daemon is its only holder, so two store instances can write the same segment without cross-instance serialization. Decode failures then affect individual session records.

The TUI polls the complete session list every second and replaces its current model after every successful response. Active-store scans currently tolerate undecodable records by skipping them, which turns corruption into a successful but incomplete response. As different records become readable or receive newer updates, agents disappear and reappear.

## Goals

- Make the daemon the sole owner of the live session database.
- Ensure an active-fleet read is complete or explicitly fails; never publish a silent partial snapshot.
- Preserve the last known-good fleet in the TUI during transient or storage failures.
- Provide safe, offline tooling to diagnose and repair an existing damaged session store.
- Recover the current installation without losing tmux sessions, dirty worktrees, branches, or archived history.

## Non-goals

- Redesigning agent lifecycle semantics or project grouping.
- Automatically deleting orphaned sessions, worktrees, or branches during repair.
- Treating a TUI-only workaround as sufficient remediation for database corruption.

## Phase 1 — Reproduce the failures

Add deterministic regression coverage before changing behavior.

### Store reproduction

- Open a daemon-owned `FileStore` in a temporary data directory.
- Attempt to open a second writable store against the same directory.
- Under the current implementation, run concurrent updates from both instances and demonstrate segment corruption or inconsistent reads.
- Preserve a corrupt-segment fixture representative of the observed failure for read and repair tests.

### TUI reproduction

- Feed the control pane a complete fleet snapshot.
- Follow it with a nominally successful partial snapshot.
- Follow that with the complete snapshot again.
- Assert that the current implementation removes and restores rows, reproducing the visible flicker.

### Exit criteria

- Tests reliably demonstrate the unsafe multi-owner store and fleet flicker.
- Fixtures contain no user prompts, tokens, paths, or other sensitive production data.

## Phase 2 — Enforce single ownership of the live store

Make the daemon the only normal process allowed to mutate active session data.

### Daemon hot-swap API

- Add a daemon endpoint and executor operation for backend/model hot-swap.
- Perform lifecycle hot-swap and session persistence inside the daemon.
- Serialize the final session update through the daemon-owned `FileStore`.
- Publish the resulting session change through the normal notification path.
- Return the resulting backend, model, handoff path, and updated session metadata.

### CLI migration

- Change `warden switch` to call the daemon endpoint.
- Remove its direct `store.NewFileStore` write.
- Remove direct-store fallback for live session lookup.
- If the daemon is unavailable, fail clearly instead of opening the live database.
- Audit non-test `NewFileStore` call sites and migrate any other online CLI access behind daemon APIs.

### Defense-in-depth ownership lock

- Acquire an exclusive lock for writable session-store ownership.
- Keep the lock for the lifetime of the daemon store.
- Return a typed `store already owned` error when a second writer opens it.
- Allow offline maintenance only after the daemon releases the lock.
- Document lock recovery for a genuinely stale owner without weakening the normal exclusion guarantee.

### Exit criteria

- No normal CLI command opens a second writable live store.
- A second writer is rejected deterministically.
- Hot-swap remains resumable and updates the same agent/worktree through the daemon.

## Phase 3 — Make active scans atomic from the caller's perspective

An incomplete scan must never be treated as an authoritative fleet snapshot.

### Store semantics

- Track decoding, checksum, and segment-read failures during collection scans.
- For the active collection, return a typed degradation error if any record cannot be read.
- Do not return a partial active list alongside a nil error.
- Include collection, segment, offset or key when available, and failure class in structured diagnostics.
- Rate-limit repeated warnings from poller and list traffic.

### Archive policy

- Decide explicitly whether history scans remain tolerant.
- If corrupt archive records are skipped, expose a degraded flag and skipped-record count.
- Never present a degraded archive export as a complete backup.

### API and SSE behavior

- Return a non-success response for degraded active REST reads.
- Do not publish an SSE snapshot when the underlying active scan is degraded.
- Keep the previous SSE snapshot until a complete scan succeeds.
- Add a store-health endpoint or health field suitable for TUI and operator diagnostics.

### Exit criteria

- Active reads return the full fleet or an explicit error, never a silent subset.
- REST and SSE agree on completeness semantics.

## Phase 4 — Make the TUI resilient

Preserve operator visibility when the daemon or session store is unhealthy.

### Snapshot handling

- Retain the last complete session snapshot when polling fails.
- Retain it when the daemon reports store degradation.
- Record the timestamp of the last complete snapshot.
- Remove an agent only after a later complete authoritative snapshot omits it.

### Operator feedback

- Display a persistent but non-blocking warning such as:

  `session store degraded · showing last complete fleet from 18:04:31`

- Distinguish daemon disconnection, request timeout, and store degradation.
- Clear the warning only after a complete read succeeds.
- Preserve cursor position, expanded groups, opened agent, and scroll position during degraded periods.

### Polling and SSE

- Apply identical last-known-good rules to polling and SSE consumers.
- Use bounded reconnect backoff for dropped streams.
- Avoid clearing state while reconnecting or re-authenticating.

### Exit criteria

- A simulated store failure does not remove agent rows.
- The TUI communicates that displayed data may be stale.
- Normal refresh resumes without layout churn after recovery.

## Phase 5 — Add offline diagnosis and repair tooling

Provide a supported alternative to manually editing append-only segments.

Suggested commands:

```text
warden doctor --sessions
warden repair sessions --dry-run
warden repair sessions --backup <path> --apply
```

### Diagnosis

- Verify the daemon/store ownership lock state.
- Validate segment framing, JSON payloads, checksums, indexes, and record ordering.
- Report affected collections, records, segments, and recoverability.
- Compare active metadata with live tmux sessions and known worktrees.

### Repair

- Require the daemon to be stopped before applying changes.
- Create a timestamped backup of the complete warden data directory or all session-store inputs.
- Reconstruct collections into a new directory rather than editing segments in place.
- Retain the newest valid record for each session.
- Preserve valid active and closed records.
- Produce a machine-readable and human-readable recovery report.
- Atomically replace the database only after validation succeeds.
- Leave the original backup intact and print rollback instructions.

### Reconciliation

- Report live tmux sessions missing from recovered active metadata.
- Offer a separate, explicit adoption step; do not adopt or delete automatically.
- Report dirty, unpushed, and unowned worktrees without modifying them.

### Exit criteria

- Dry-run performs no writes.
- Apply is backup-first, atomic, and idempotent.
- Interrupted repair leaves the original database usable or recoverable.

## Phase 6 — Recover the current installation

Run only after the repair tooling and its tests are available.

1. Stop the daemon gracefully and verify the ownership lock is released.
2. Back up `/home/srjn45/.warden` with metadata and permissions preserved.
3. Run `warden repair sessions --dry-run`.
4. Review recovered, skipped, duplicate, and ambiguous records.
5. Apply reconstruction to a new store directory.
6. Validate the reconstructed database before activation.
7. Restart the daemon.
8. Reconcile recovered active records with tmux sessions and worktrees.
9. Verify `warden ls`, individual status reads, REST/SSE snapshots, Cockpit refresh, and autopilot state.
10. Retain the backup until the fleet and history have been independently verified.

No tmux session, worktree, branch, or archived record should be deleted as part of this phase.

## Phase 7 — Integration and regression coverage

Required coverage:

- Second writable store opener is rejected.
- `warden switch` uses the daemon API.
- Switch fails safely when the daemon is unavailable.
- Concurrent poller and hot-swap updates remain consistent.
- A corrupt active record causes an atomic list failure.
- SSE does not publish a partial snapshot.
- TUI retains the last complete fleet and displays degradation status.
- Archive degradation is visible in history/export behavior.
- Repair dry-run performs no mutation.
- Repair creates a valid backup and supports rollback.
- Repeated repair is idempotent.
- Live tmux sessions and dirty worktrees are reported for reconciliation.

Run race-enabled and repeated stress tests around spawn, poller updates, hot-swap, archive, restore, and fleet listing. Include a test that repeatedly refreshes the TUI while these operations run.

## Delivery order

1. Store ownership lock and daemon hot-swap API.
2. CLI migration and direct-store access audit.
3. Atomic active-scan semantics and health reporting.
4. TUI and SSE last-known-good behavior.
5. Offline repair tooling.
6. Current-installation recovery.
7. Stress testing and independent review.

## Rollout and rollback

- Ship ownership enforcement before using hot-swap again in production.
- Keep the previous session database untouched during recovery.
- Gate repair application behind explicit confirmation and a successful backup.
- If reconstructed-store validation fails, restore the original directory and leave the daemon stopped for investigation.
- Do not downgrade to silent partial reads as a rollback; retain explicit degradation even if the UI enhancements must be reverted.

## Definition of done

- Hot-swap cannot create a second writer to the live session store.
- Active fleet reads are complete-or-error.
- TUI rows do not disappear during transient or storage failures.
- Operators receive clear last-known-good and store-health information.
- The damaged installation can be repaired with a supported, backup-first workflow.
- All unit, integration, race, and stress checks pass.

## Implementation status (2026-08-31)

### Implemented and covered

- The daemon-held cross-process ownership lock rejects a second writable store;
  `warden switch` now uses the daemon API and has no live-store fallback.
- Active scans are complete-or-error, REST maps degradation to a non-success,
  SSE suppresses incomplete snapshots, and store health exposes structured
  failures with rate-limited warnings.
- The Cockpit TUI retains the last complete fleet across degraded, timed-out,
  and disconnected polls, preserves selection/layout state, and shows a
  persistent last-known-good banner. CLI SSE watch reconnects with bounded
  backoff without clearing its last rendered snapshot.
- Offline `doctor --sessions` and `repair sessions` diagnose raw segments and
  reconcile active/closed duplicates. Apply requires an explicit external
  backup path, re-diagnoses under the ownership lock, validates a fresh rebuild,
  and installs it with a recovery journal so a crash between directory renames
  restores the original on the next store open. Backups retain regular-file
  permissions/timestamps and symlinks; partial backups are removed on failure.
- Archive history responses explicitly report `degraded` and
  `skipped_records`; hot-swap persistence failures are no longer discarded.
- The OpenAPI server and generated CLI reference have been regenerated from
  their source specifications.

### Verified without operator-data access

- Unit/integration coverage uses temporary directories and synthetic corrupt
  entries. It covers ownership refusal, strict scans, SSE suppression, TUI
  retention/recovery, dry-run non-mutation, backup/rebuild/idempotence,
  interrupted-archive reconciliation, and interrupted-repair startup recovery.
- Repository build, formatting, vet, tests, generated-file checks, and selected
  race/stress checks are the release gate for this branch. No repair or diagnosis
  command was run against `~/.warden`.

### Intentionally not executed / remaining operational work

- Phase 6 (repairing the current installation) is intentionally not performed
  by this implementation/review branch. It requires an operator-approved outage,
  an independently inspected dry-run report, and retention of the external
  backup until live fleet/history/TUI/autopilot verification completes.
- The replacement is crash-recoverable rather than a single syscall on every
  supported filesystem: portable directory replacement requires two renames.
  The fsynced journal closes that gap by restoring the old DB when installation
  did not finish and completing cleanup when it did.
- Non-Unix builds compile with a documented best-effort lock stub; warden's
  supported Linux/macOS targets use kernel-backed `flock` exclusion.
