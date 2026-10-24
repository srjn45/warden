# Autopilot manager-slot recovery contract

**Status:** Contract and deterministic fixture baseline for
`recovery-contract-and-fixtures`.  This document deliberately does not add an
adoption path.  `verified-manager-adoption` owns that implementation.

## Scope and safety boundary

Recovery reconciles one deterministic manager slot,
`<slot_scope>-autopilot`, for one persisted run.  It is not a general tmux
discovery mechanism.  A recovery attempt must never kill, rename, attach to, or
write state for a pane unless the pane has already passed the evidence below.
Workers, their worktrees and branches, and non-manager child sessions remain
untouched in every outcome.

All external observations (store, tmux, filesystem, ledger and plan service)
run outside the controller lock and have a deadline.  Per-run recovery is
single-flight; a bounded global admission limit prevents a stuck dependency
from monopolising the daemon.  Completion releases admission and records a
success, refusal, timeout, or cleanup counter.  A retry is admitted only after
the persisted backoff deadline, except for one observed repair of a
plan-definition failure.  Duplicate-session errors are an observation signal
for one verification pass, never permission to retry a spawn loop.

## Minimum adoption evidence

All of these facts are required.  Missing, conflicting, stale, or unreadable
evidence is a refusal, not a best-effort adoption.

| Evidence | Required value | Why it is necessary |
|---|---|---|
| Stable slot | observed tmux name equals the run's derived manager slot | prevents arbitrary-pane adoption |
| Liveness | a bounded tmux liveness observation succeeds | avoids creating a record for a dead pane |
| Run ownership | authenticated launch/session metadata names exactly one run id and plan id | prevents cross-run reuse of a slot-shaped pane |
| Repository ownership | canonical repo identity equals the persisted run repo/project | prevents adoption from another checkout |
| Manager role | metadata says `autopilot` manager, not worker, shell, guardian, or resolver | preserves role and authority boundaries |
| Session continuity | backend/session identity is present and agrees with the launch metadata | makes later hot-swap and audit attribution possible |
| No competing record | no other live manager record claims the run or slot | prevents split-brain managers |

The verifier may use an existing registered manager record as corroborating
evidence, but a record alone does not prove that tmux is live.  Conversely, a
live pane name alone never proves ownership.  A legacy record with incomplete
metadata is not silently upgraded; it yields `verification_failed` with the
missing evidence listed.

## Reconciliation outcomes and durable transitions

| Fixture class | Result | Run transition | Ledger/audit transition |
|---|---|---|---|
| live registered | retain the existing record; refresh only safe liveness observations | active/healing remains unchanged; no spawn | append idempotent `manager_reconciled` observation; ledger task state unchanged |
| live unregistered, verified | register the complete manager slot record and attach it to the run without launching a pane | degraded/healing → active after normal readiness evidence | append `manager_adopted` with evidence fingerprint; ledger task/landings preserved |
| dead slot | do not adopt; archive/clear only a stale Warden record through the supported lifecycle | healing or degraded/backoff; normal bounded spawn policy decides later | append `manager_missing`; ledger preserved |
| foreign slot-shaped pane | do not attach, kill, rename, or mutate it | degraded with `verification_failed: foreign ownership` | append refusal audit only; ledger untouched |
| ambiguous evidence | do not attach or spawn a rival manager in this pass | degraded with an actionable list of conflicts | append refusal audit only; ledger untouched |

All writes are idempotent by `(run_id, manager_slot_id, observation epoch)`.
The adoption write creates the full session record atomically enough that a
reader never sees a manager assigned to the run without slot, run/plan/project,
role, backend, workdir/repo, tmux identity, and ownership tags.  It must retain
the pane's existing tmux name.  It must not infer a worker's parentage or alter
the canonical task ledger.

## Plan and journal recovery

Canonical `plan_id`/project/run metadata remains authoritative when a persisted
`plan_file` is empty, invalid, or no longer readable.  Recovery first tries to
hydrate from that canonical metadata.  If it cannot, it records
`plan_definition_repair_needed` with the precise missing source and makes no
duplicate manager launch.  A verified repair permits one admission attempt;
unchanged failure remains governed by backoff.

Journal corruption is recovered only through the supported backup,
quarantine, and rebuild/repair operation.  The original bytes and failure
diagnostic are retained in the supported quarantine location before rebuild.
Recovery never edits a store, journal, or ledger file directly.  If repair is
unavailable or fails, status is `journal_repair_needed`, manager adoption and
worker records remain intact, and the operator gets the repair command/path.

## Child preservation

This contract distinguishes Warden-owned test/runtime descendants with a
recorded ownership marker from production child sessions.  Cancellation and
test cleanup may reap only marked, run-owned descendants within a deadline and
report failures.  Production PTY sessions with valid re-adoption evidence are
preserved across daemon restart; recovery must not use broad tmux, process,
server, cgroup, or worktree cleanup.

## Fixture corpus

`internal/autopilot/testdata/manager_reconciliation_cases.json` is the stable,
process-free corpus for this contract.  Tests inject its observations into fake
store/tmux/metadata adapters; they must not require a daemon, a real tmux
server, or persisted production data.  Implementations may add fields, but
must preserve these five named cases and their expected dispositions.

