# Usage API Quota Recovery — Phase 10 Acceptance Report

**Date:** 2026-10-02  
**Plan:** `plan-69eb481d` / task `migration-docs-acceptance`  
**Design freeze:** [`2026-09-29-usage-api-quota-recovery.md`](./2026-09-29-usage-api-quota-recovery.md)  
**Gate tests:**
- `TestUsageAPIQuotaRecovery_Phase10Acceptance`
- `TestUsageAPIQuotaRecovery_Phase10UsageFirstBothAgents`
  in `internal/daemon/usage_quota_recovery_acceptance_test.go`
- Compatibility:
  - `TestHotSwapAcquiresQuotaBindingForUnboundLegacy`
  - `TestRestoreLeavesUnboundLegacyWithoutGuessing`
    in `internal/lifecycle/quota_binding_compat_test.go`
  - `TestLegacySessionWithoutBindingStaysUnbound` (agentstore)
  - `TestReconcileImpactSkipsIneligibleAndUnbound` (capacity)

## Verdict: GREEN

All Phase 10 acceptance criteria are covered by the integrated gates plus the
focused package suites from Phases 1–9. This phase adds compatibility proofs,
operator playbooks/docs, and the dual-Claude weekly-limit regression fixture —
no second recovery coordinator and no guessed mass-swaps for unbound legacy
agents.

## Representative corpus

| Fixture | Purpose |
|---|---|
| Two Claude agents bound to shared `AccountFingerprint` + `weekly` mandatory bucket | Motivating shared-domain incident |
| Claude wait-menu selection → `Requesting rate limit reset for weekly limit` | Confirmed menu evidence with **no** parseable `resets` banner |
| Fresh authoritative usage snapshot for the shared weekly bucket | Authoritative capacity signal |
| Unbound legacy peer (`backend`/`model` only) | Prove `unbound_legacy` is never mass-swapped |
| Concurrent menu + usage / usage-then-menu arrival orders | Generation idempotency |

## Assertion table

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Older agents with only backend/model keep working as `unbound_legacy` | **PASS** | Gate + `TestLegacySessionWithoutBindingStaysUnbound` |
| 2 | Unbound agents acquire a binding on next safe HotSwap | **PASS** | `TestHotSwapAcquiresQuotaBindingForUnboundLegacy` |
| 3 | Restore does not guess account/bucket for unbound agents | **PASS** | `TestRestoreLeavesUnboundLegacyWithoutGuessing` |
| 4 | Unbound agents are never mass-swapped from guessed domain data | **PASS** | Gate skip reason `unbound_legacy`; impact unit test |
| 5 | Two Claude agents + wait menu + no banner still recover | **PASS** | `TestUsageAPIQuotaRecovery_Phase10Acceptance` |
| 6 | Shared weekly usage exhaustion selects both bound agents | **PASS** | Usage-first twin gate |
| 7 | Recovery starts once per agent via existing coordinator | **PASS** | Generation `== 1`; swaps through `OnHardLimit` only |
| 8 | Simultaneous menu + usage evidence does not duplicate generations | **PASS** | Concurrent dual-source block in acceptance gate |
| 9 | Operator docs/playbooks cover polling, dry-run, unknown providers, waiting, override | **PASS** | Site guide + USAGE + FEATURES + release notes |

## Focused suites (run as part of acceptance)

| Surface | Package / tests |
|---|---|
| Capacity / impact | `./internal/capacity/` |
| Lifecycle binding | `./internal/lifecycle/` (`quota_binding_compat_test.go`) |
| Pane menu fusion | `./internal/poller/` (`limit_menu_fusion_test.go`) |
| Bulk recovery | `./internal/daemon/` (`usage_bulk_recovery_test.go`) |
| Pane↔usage fusion | `./internal/daemon/` (`pane_signal_fusion_test.go`) |
| Operator recover | `./internal/daemon/` (`usage_recover_test.go`) |
| Observability | `./internal/daemon/` (`recovery_observability_test.go`) |
| Phase 10 gate | `./internal/daemon/` (`usage_quota_recovery_acceptance_test.go`) |
| Repo check | `make verify-fast` / `wd check` |

## Design-freeze checklist

- [x] Provider usage API is authoritative for bound agents when fresh + mandatory bucket exhausted
- [x] Pane menu/banner remains low-latency supplemental evidence
- [x] Unbound legacy agents remain operable and ineligible for bulk reconciliation
- [x] Binding acquisition only on daemon-owned safe transitions (spawn / HotSwap); Restore never guesses
- [x] One recovery generation per incident across usage/menu/banner/manual sources
- [x] Manual switch/stop/delete supersedes automatic recovery
- [x] Credentials / raw account ids never appear in bindings, audit, or API

## Explicitly out of scope for this phase

| Item | Owner |
|---|---|
| Tag / public release cut | Parent orchestrator after human approval of this PR |
| Successor `ExecutionProfile` / loopback (#606) | Already on main — docs must not regress it |
