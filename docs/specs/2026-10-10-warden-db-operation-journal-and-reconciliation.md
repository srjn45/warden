# Warden Schema-2 Operation Journal and External-Effect Reconciliation Contract

**Date:** 2026-10-10
**Status:** Spec freeze / Contract — **design only** (Plan `01-warden-db-architecture-and-contract`, Task `operations`)
**Integration branch:** `autopilot/01-warden-db-architecture-and-contract`
**Depends on:** [data-source inventory](2026-10-10-warden-db-data-source-inventory.md) and [Schema-2 collection contract](2026-10-10-warden-db-collection-contract.md), especially §§1.2, 1.4, 5–6.
**Scope:** Define the durable operation journal, saga/reconciliation protocol, and recovery rules for effects outside `warden-db`. This is not an implementation or a change to production paths or schemas.

Keywords **MUST**, **MUST NOT**, and **MAY** are normative.

---

## 1. Decision and boundary

Schema-2 uses one ScrivaDB instance, `<dataDir>/warden-db`, and its
cross-collection transaction facility is **required** for every internal
multi-collection invariant. It is not a distributed transaction coordinator.
Tmux, Git, native CLI processes, providers, and the filesystem are external
resources and MUST NOT be represented as participants in, or assumed atomic
with, a ScrivaDB transaction.

Accordingly, every durable intent to create, mutate, or remove an external
resource MUST be represented by an operation-journal record before the effect
is attempted. The durable journal plus reconciliation is the saga boundary:

```
DB transaction: validate + reserve identity + write operation intent
                         |
                         v
                 external effect (at-least-once)
                         |
                         v
DB transaction: record observed result + advance dependent DB state
```

There is deliberately no claim of exactly-once execution. The contract
provides exactly-once *logical completion* where an external API supports an
idempotency key, and otherwise at-least-once execution with deterministic
observation and convergence.

### 1.1 Internal transaction rule

The transaction that creates or advances an operation MUST include every
internal change that makes the operation necessary: for example, an agent
reservation and its `tmux.create` operation; a job-to-agent binding and its
`git.worktree.create` operation; or snapshot metadata and its transcript-write
operation. Foreign-key validation, uniqueness claims, status transitions, and
the operation record commit together or all roll back.

No DB transaction may hold a filesystem lock, a Git lock, a tmux command, a
native CLI process, or a provider request open. Conversely, an external effect
MUST NOT be issued before its committed operation is visible to recovery.

### 1.2 Contract amendment: `operations` collection

The collection catalogue in the Schema-2 collection contract is extended by a
new durable `operations` collection owned by `internal/operationstore` (or its
eventual schema-2 adapter). It is a durable journal with immutable intent and
append-only attempt/audit history; its workflow fields are deliberately
mutable. It is not an inverse membership list and not a replacement for the
authoritative domain records it references. Implementations MUST add it to the collection
registry, backup/import rules, and validator before using this design.

Key: `op-<ULID>`; immutable. Required indexed fields are `kind`, `state`,
`subject_type`, `subject_id`, `idempotency_key`, `next_attempt_at`,
`created_at`, and terminal `finished_at`. The unique key is
`(kind, idempotency_key)` for the lifetime of retained journal rows.

| Field | Class | Contract |
|---|---|---|
| `id`, `kind`, `subject_type`, `subject_id`, `intent`, `idempotency_key`, `depends_on`, `created_at`, `deadline_at`, `compensation` | A | Immutable intent/identity. `depends_on` contains earlier operation ids and is checked transactionally; `intent` includes only stable, non-secret desired state; secrets are references to the approved secret mechanism, never journal values. |
| `state`, `attempt`, `next_attempt_at`, `lease_owner`, `lease_expires_at`, `last_error`, `finished_at`, `supersedes` | A | Journal coordination and outcome. Each change is transactional and fenced by the operation version/CAS. |
| `observation`, `observed_at`, `external_ref`, `result` | O | Last verified external fact. `external_ref` is the resource identity used by reconciliation. |
| `updated_at` | D | Store-stamped. |

`subject_type` + `subject_id` is an auditable reference, not necessarily a
strong FK: historical operations MUST survive archival and permitted purges.
The operation creator validates a live subject when one is required; repair
reports a later dangling reference. `intent` is canonicalized before deriving
the idempotency key. Large output, transcripts, or provider payloads MUST be
stored as external blobs with a bounded pointer/checksum in `result`, never in
the journal row.

Retention: retain terminal rows for the applicable audit/recovery period
(default at least 30 days; destructive and provider-billing rows at least the
system audit retention). A pruner MAY delete only terminal rows whose subject
has passed its retention period and whose idempotency key is no longer needed;
it MUST never delete a non-terminal row.

## 2. State machine, claiming, and idempotency

An operation moves monotonically except that `retry_wait` returns to
`ready`. Terminal states never change; a new desired action creates a new
operation linked with `supersedes`, rather than rewriting intent.

| State | Meaning | Allowed next states |
|---|---|---|
| `pending` | Committed intent; prerequisites/dependencies not yet satisfied. | `ready`, `canceled`, `failed` |
| `ready` | Eligible for an executor claim. | `running`, `canceled`, `failed` |
| `running` | A leased worker may issue or observe the effect. A crash may leave this state. | `succeeded`, `retry_wait`, `reconciling`, `compensating`, `failed` |
| `retry_wait` | A retryable failure has a persisted due time. | `ready`, `failed`, `canceled` |
| `reconciling` | Recovery is determining whether the desired resource already exists, is absent, or conflicts. | `succeeded`, `ready`, `compensating`, `manual_intervention`, `failed` |
| `compensating` | A compensating action is being observed/issued. | `compensated`, `retry_wait`, `manual_intervention`, `failed` |
| `succeeded` | Desired external state was verified, then dependent DB state was committed. | terminal |
| `compensated` | Verified inverse/best-safe cleanup completed. | terminal |
| `canceled` | Intent was canceled before issuance, or absence was verified as the desired result. | terminal |
| `failed` | Non-retryable failure with no safe compensation or retry. | terminal |
| `manual_intervention` | Automation cannot safely choose because observation conflicts with intent. | terminal until an operator creates a successor operation |

A worker claims `ready` work by a compare-and-set transaction that increments
`attempt`, assigns a random `lease_owner`, and sets a bounded
`lease_expires_at`. Only the holder may write its outcome. A lease is a liveness
hint, not proof that no effect was issued: an expired `running` row MUST enter
`reconciling`, never be blindly rerun.

The idempotency key is opaque, stable for the logical desired effect, and MUST
be deterministic from the schema-2 operation type, durable subject identity,
desired resource identity, and a semantic generation/revision. It MUST NOT
include timestamps, attempt counts, process IDs, or mutable display names. A
caller retries the same intent by looking up `(kind, idempotency_key)`; an
intent whose desired state changes mints a new key. Where an external system
accepts a key (notably provider APIs), it MUST receive this key or a bounded,
documented derivation of it.

## 3. Effect-family contracts

| Family | Operation kinds and external reference | Apply/idempotence rule | Reconciliation and compensation |
|---|---|---|---|
| **tmux** | `tmux.session.create`, `.kill`, `.send`; `external_ref` is the exact socket/session/pane identity. | Create uses a reserved, unique session name. Treat “already exists and matches expected command/workdir/ownership marker” as success; never attach an arbitrary same-named session. Kill/send target the exact recorded pane. | List/query tmux after every ambiguous result. A matching owned session completes create; absence completes kill. A foreign/conflicting session is `manual_intervention`. Compensation for a failed launch is owned-session kill only. |
| **Git** | `git.worktree.create/remove`, `git.branch.create/delete`, `git.fetch`, `git.stash.create/drop`, `git.pr.create`; ref includes canonical repo path, worktree path/ref/branch or remote PR identity. | Use deterministic worktree/branch names recorded before apply. Inspect Git state under the repository's normal lock; “already present with expected HEAD/upstream/path” is success. Never force-push, delete a ref, or remove a worktree merely because its name matches. | Reconcile with `git worktree list --porcelain`, ref/upstream/HEAD inspection, and remote PR lookup. Cleanup is limited to resources marked owned by this operation. Divergent branch, changed HEAD, foreign worktree, or existing PR with incompatible head/base requires intervention. |
| **native CLI** | `cli.launch`, `.signal`, `.terminate`, `.handover`; ref is agent id plus tmux pane and, once known, child PID/session id. | Launch through its tmux operation dependency; pass the operation/agent correlation token via approved environment or staging reference. Repeating a launch first discovers a live correlated process/session. Signals are idempotent by desired terminal state. | Observe process tree, tmux pane, exit marker, and CLI session id. An exited/absent process completes termination; an unknown live process is not killed automatically. Compensation is a graceful terminate followed by bounded escalation only for a correlated, owned process. |
| **provider** | `provider.request`, `.cancel`, `.usage.refresh`; ref is provider request/job/conversation id and account/domain (non-secret). | Supply the journal key to provider idempotency support. Without it, persist the client request correlation before send and reconcile by provider-side request/job lookup; a charge-creating or irreversible call with no safe lookup MUST stop at `manual_intervention` after an ambiguous failure. | Query provider status/rate-limit/usage endpoint. Cancellation is attempted only when provider documents it and the request is still cancellable; it is not assumed to undo billed or emitted work. Provider observations remain O-class data. |
| **filesystem** | `fs.mkdir`, `fs.atomic_write`, `fs.rename`, `fs.remove`, `fs.blob.write/remove`, staging file operations; ref is a canonical, permitted path plus expected owner/checksum. | Create parent safely; write immutable content to a same-filesystem temporary name, fsync as required, then atomic rename. An existing destination is success only if checksum/owner matches. Delete is idempotent only for a recorded owned path and expected identity. | `lstat` plus ownership marker, checksum, and expected type determine convergence. Never recursively remove a broad directory or a mismatching/symlink-substituted path. Compensation removes only the exact owned artifact, after revalidation. |

An operation may depend on predecessor operation ids. Dependency satisfaction is
checked in the transaction that advances `pending` to `ready`; failed or
manual predecessors block dependents with a diagnostic rather than allowing an
unsafe partial launch.

## 4. Retry, compensation, and reconciliation policy

Retries are driven by persisted `next_attempt_at`, not in-memory timers.
Retryable errors include transient transport/process unavailability,
explicit provider throttling with a reset time, and verified lock contention.
The executor uses capped exponential backoff with jitter and honors a declared
provider reset/deadline. Permission denials, invalid intent, violated ownership
markers, and incompatible observed state are non-retryable.

Before an apply attempt after any ambiguous outcome, the executor MUST
reconcile first. This includes timeout, connection reset after request write,
daemon restart, process crash, lease expiry, and failure between external
success and DB outcome recording. It MUST prefer observation over reissue:

1. If the desired state is observed and matches the intent, transactionally
   write `observation` and mark `succeeded` with the related DB state.
2. If the pre-effect/absent state is observed, return to `ready` or apply a
   safe compensating operation as the intent requires.
3. If an owned partial resource exists, finish it or compensate according to
   the operation's declared policy.
4. If the resource exists but ownership, identity, or semantic state conflicts,
   record the evidence and enter `manual_intervention`; never guess.

Compensation is itself durable: create a linked operation with its own key and
state rather than executing cleanup as an unrecorded `defer`. It is
best-effort, idempotent, and narrowly scoped to operation-owned resources.
Some effects are inherently non-compensable (provider billing, a user-visible
remote message, an already-merged Git PR); their contract is reconciliation
and explicit terminal evidence, not fictional rollback.

## 5. Crash recovery and boot reconciliation

On daemon startup, before admitting new conflicting work, the reconciler MUST
scan all non-terminal operations and rows with expired leases. It claims work
using the same CAS lease mechanism and processes it in dependency order.

- `pending` rows are re-evaluated against their committed prerequisites.
- `ready` and `retry_wait` rows become eligible only when due.
- `running` rows with a live lease are left to their owner; those with an
  expired lease move to `reconciling`.
- `reconciling` and `compensating` rows resume observation, never blindly
  repeat the last command.
- A completed external observation is paired with its internal follow-up in
  one ScrivaDB transaction; if that transaction was the crash point, recovery
  repeats the observation and commits the missing internal result.

The domain reconcilers (agent/tmux, Git/worktree, pipeline/job, provider,
snapshot/filesystem) MAY feed observations into `operations`, but only the
operation owner advances its state. Periodic reconciliation continues after
boot for due retries and observed-state drift. It does not mutate authoritative
intent merely because an external resource disappeared: it records the
observation and either recreates through the existing intent or moves the
operation/subject to a documented degraded state.

## 6. Required invariants and examples

1. **Record-before-effect:** no external mutation without a committed journal
   row; no operation intent references an uncommitted agent/job/run binding.
2. **Transactional internal completion:** marking success and advancing all
   dependent DB records is one ScrivaDB cross-collection transaction.
3. **Ownership before destructive action:** reconciler cleanup is allowed only
   with exact operation ownership plus type/path/session verification.
4. **Observable outcome:** every terminal journal row records a bounded result
   or error and an `observed_at` time where external observation was possible.
5. **No hidden retry:** retry count, due time, and last error are durable;
   restart cannot reset a retry budget or forget an ambiguous issue.

For agent launch, one transaction creates the valid `agents` row in
`starting`/O-class runtime state, records the reserved tmux identity, and adds
`tmux.session.create`. Reconciliation then observes or creates tmux. A
successful observation is committed with the agent runtime observation; only
then may dependent `cli.launch` become ready. A crash at any point yields a
known DB intent and a resource that can be discovered, not an orphan inferred
from a missing in-memory callback.

For a Git worktree, the transaction reserves the worktree/branch identity and
records `git.worktree.create`. Reconciliation verifies the exact path/ref and
ownership before success. If create succeeded but the daemon died before the
success transaction, the next run recognizes the same worktree as success. If
the path is occupied by a different worktree, it stops for intervention rather
than deleting or reusing it.

## 7. Security, observability, and acceptance requirements

The journal MUST redact credentials, provider prompts/responses that policy
classifies as sensitive, and raw terminal output. It stores stable references,
checksums, typed errors, and bounded diagnostic excerpts. Operator/API views
MUST expose operation id, subject, state, attempt/due time, external reference,
and remediation reason; access to detailed diagnostics follows existing audit
and secret policies.

The implementation acceptance suite MUST demonstrate at least:

- atomic creation of a journal row with every multi-collection internal
  reservation, and rollback of both on validation failure;
- crash injection before effect, after effect/before result commit, and during
  compensation for each effect family;
- repeated delivery of an operation and expired lease recovery without duplicate
  owned tmux sessions, Git worktrees/branches, filesystem blobs, or provider
  submissions where provider idempotency exists;
- conflict fixtures proving a foreign tmux session, Git worktree/ref, process,
  provider job, or filesystem path reaches `manual_intervention` rather than
  being destroyed;
- persistence of retry budget/backoff across restart, and retention/pruning
  refusal for non-terminal rows;
- validation that no external command is called while a ScrivaDB transaction is
  open.

This contract intentionally does not prescribe worker queues, exact Go types,
or migration code. Those are implementation decisions constrained by the
durable state machine and boundaries above.
