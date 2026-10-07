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
| Offline ownership probe and deliberately unavailable repair capability | `internal/agentstore/repair_test.go`, `internal/cli/repair_agents_test.go` |
| Store-health REST contract and degraded verdict | `internal/daemon/store_health_test.go` |
| Membership conflicts are reported and skipped rather than reconciled | `internal/daemon/membership_identity_test.go`, `internal/daemon/testdata/incident_membership.json` |
| Daemon/doctor process-level and safe-start behaviour | `internal/e2e/recovery_test.go` and the agent-store contract tests |

`wd check` is the project-level gate and runs the repository's configured
format, generation, lint, test, web, and release checks.

## ScrivaDB dependency boundary

The pinned dependency is `github.com/srjn45/scriva v1.2.1`. It does not export
the Verify/Repair primitives Warden needs to inspect segments, distinguish
ambiguous histories, and atomically rebuild indexes. The dependency contract is
pinned by `TestContractScrivaDependencyPin` in
`internal/agentstore/contract_test.go`; any ScrivaDB upgrade must re-audit this
report and the contract design record.

Until upstream support exists, `agentstore.RepairAvailable` is false and
`warden inspect repair agents` is a non-mutating preflight only. This is a
deliberate safety boundary, not a failed repair attempt.

## Residual risks and follow-up

- The original second writer is unproven. The incident shape supports candidate
  paths but is not evidence that either was the production trigger.
- Filesystems without reliable advisory-lock semantics are unsupported.
- The ownership lock protects `agents-db`; other independently opened
  ScrivaDB-backed Warden stores require their own follow-up audit.
- A future ScrivaDB Verify/Repair release must receive separate offline-repair,
  interruption, backup-retention, and conflict-report validation before the
  repair capability can be enabled.
