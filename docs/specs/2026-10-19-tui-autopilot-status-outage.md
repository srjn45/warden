# Autopilot visibility during status outages

## Problem

The control pane fetches sessions and autopilot status independently. A failed
autopilot request can coexist with a successful session snapshot. The shared tree
claims explicitly run-owned sessions before creating run containers; if the TUI
has no corresponding run status, those sessions have no visible parent and vanish.
This is especially misleading on a newly opened TUI with no last-good run cache.

## Implementation plan

1. Add a presentation-only reconciliation step before building the TUI tree.
   Preserve API run records. For missing runs, create minimal unavailable rows
   from explicit session run back-references and legacy ownership tags. Prefer
   manager metadata deterministically; use explicit project/plan links where
   available. Do not interpret a process/session's existence as executor health.
2. Track autopilot-fetch errors independently from ordinary fleet refresh health.
   Retain the last successful response but label its displayed state unavailable
   until the next successful status response. Display a persistent header warning.
3. Keep manager/worker rows navigable and attachable. Unavailable run rows must
   not present invented zero task counts or cached final-PR/health claims, nor
   dispatch pause/resume/stop using unverified state. Explain how to access the
   children instead of opening an overview known to require the failed endpoint.
4. Add regression coverage for startup failure, cached refresh failure, recovery,
   independent response ordering, mixed runs/projects, worker-only runs, ownership
   tags/back-references, rate-limited agents, historical records, and run actions.
   Run the existing TUI tests under race detection plus configured project checks.

## Boundaries

- Inferred rows exist only in memory; no database writes, status transitions,
  agent launches, cleanup, or daemon restart are part of this change.
- Done, errored, and orphaned historical session records alone do not create new
  fallback run rows. Rate-limited sessions do, since their execution can resume.
- Actual API records always win over inference. Fallbacks are rebuilt from the
  current session snapshot rather than being added to the authoritative cache.
- A status error never clears the last-good cache. A subsequent successful status
  response clears the error; inferred rows disappear when no current session or
  API run supports them.
- This fixes control-pane visibility, not the daemon's blocking autopilot-status
  implementation or autopilot supervision. The status endpoint's shared-lock and
  ledger access need a separate root-cause investigation and fix.
- Deployment requires the fixed TUI binary; existing running binaries are not
  changed by editing source. No release or local installation is automatic.
