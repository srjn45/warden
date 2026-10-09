# Agent-store integrity validation report (#795)

Date: 2026-10-07

## Scope

This report records the validation boundary for Warden issue #795 and its
ScrivaDB dependency. It is intentionally reproducible from this checkout; it
does not claim to reproduce or identify the original production second writer.

## Reproduce the Warden validation

From a clean checkout of the integration branch:

```sh
wd check
go test ./internal/agentstore ./internal/daemon ./internal/e2e
```

The relevant executable coverage includes:

| Behaviour | Evidence |
|---|---|
| Exclusive canonical-path ownership, aliases, release on close, and cleanup on failed open | `internal/agentstore/ownership_test.go`, `contract_test.go` |
| Read-path identity validation and complete-or-error degradation | `internal/agentstore/integrity_test.go`, `internal/e2e/degraded_test.go` |
| Offline ownership probe, dry-run and ScrivaDB-backed repair | `internal/agentstore/repair_test.go`, `internal/cli/repair_agents_test.go` |
| Store-health REST contract and degraded verdict | `internal/daemon/store_health_test.go` |
| Membership conflicts are reported and skipped rather than reconciled | `internal/daemon/membership_identity_test.go`, `internal/daemon/testdata/incident_membership.json` |
| Daemon/doctor process-level and safe-start behaviour | `internal/e2e/recovery_test.go` and the agent-store contract tests |

`wd check` is the project-level gate and runs the repository's configured
format, generation, lint, test, web, and release checks.

## ScrivaDB dependency boundary

Update (#834, p3): the pinned dependency is now `github.com/srjn45/scriva v1.4.0`,
which exports `engine.VerifyDir` and `engine.Repair`. Warden delegates verification
and repair to them (`internal/agentstore/repair.go`) and does not emulate them;
`agentstore.RepairAvailable` is true. The pin is enforced by
`TestContractScrivaDependencyPin`; any further upgrade must re-audit this report
and the contract design record. (Historical: the original report was written against v1.2.1,
which had no Verify/Repair, and shipped repair as a non-mutating preflight.)

Offline repair evidence: `internal/agentstore/repair_test.go`,
`internal/cli/repair_agents_test.go` (dry-run, JSON, refusal while owned),
`internal/e2e/recovery_test.go` and `degraded_test.go` (damaged store stays
byte-identical; repair restores it), and `internal/daemon/store_health_test.go`.

## Residual risks and follow-up

- The original second writer remains unproven. The incident shape supports candidate
  paths but is not evidence that either was the production trigger.
- Filesystems without reliable advisory-lock semantics are unsupported.
- The ownership lock protects `agents-db`; other independently opened
  ScrivaDB-backed Warden stores require their own follow-up audit.
- Repair is validated through Warden's tests plus ScrivaDB's own repair suite;
  a live-operator drill on a copy of a real damaged store is still advisable.
  Backup retention is the operator's responsibility.
