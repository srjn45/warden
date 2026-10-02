# Release notes — Usage API quota recovery

Covers Phases 1–10 of `plan-69eb481d` / `usage-api-quota-recovery` (design freeze:
`docs/specs/2026-09-29-usage-api-quota-recovery.md`).

## Highlights

- **Daemon-owned QuotaBinding** maps each AI agent to a non-secret capacity
  domain (provider + opaque account fingerprint + route + mandatory buckets).
  Agents never self-assert this value; spawn and HotSwap write it.
- **Provider usage snapshots** are the authoritative capacity signal when fresh
  and successful. Pane menu/banner detection remains the low-latency supplemental
  path (Claude wait-menu fires immediately even when no `resets` banner appears).
- **Bulk reconciliation** maps exhausted mandatory buckets to every eligible
  bound live agent and invokes the existing backend recovery coordinator —
  never a second hot-swap path.
- **`wd usage recover [--dry-run]`** (API/MCP `usage_recover`) is the explicit
  operator one-shot: always fetches fresh supported snapshots, prints impact,
  and optionally starts recovery under bounded concurrency.
- **Opt-in usage polling** via
  `rate_limit.recovery.usage_reconciliation.enabled` (default **false**). When
  disabled, the daemon makes no provider usage network calls; manual recover
  still works.
- **Legacy compatibility:** agents with only `backend`/`model` stay operable as
  `unbound_legacy`, acquire a binding on the next HotSwap, and are **never**
  mass-swapped from guessed account/bucket data.
- **Observability:** durable events + append-only audit
  (`usage_snapshot_*`, `quota_*`, `recovery_*`) with safe domain/bucket ids only.
- **Phase 10 acceptance gate** is green — dual Claude weekly-limit incident
  fixture + compatibility proofs:
  `docs/specs/2026-09-29-usage-api-quota-recovery-acceptance.md`.

## Operator playbooks

See [Backend hard-limit recovery](https://srjn45.github.io/warden/guides/backend-recovery/)
(§ Operator playbooks) and `docs/USAGE.md` (reactive backend recovery):

1. Enable / disable usage polling
2. Dry-run `wd usage recover --dry-run`
3. Unknown / unsupported providers
4. Interpreting `waiting_for_capacity`
5. Manually overriding recovery

## Surfaces updated

Configuration reference (`rate_limit.recovery.usage_reconciliation.*`), CLI
(`wd usage recover`), MCP (`usage_recover`), OpenAPI (Phase 8), `docs/FEATURES.md`,
`docs/USAGE.md`, site guide + env-vars, skill references, README What's new,
acceptance report.

## Follow-ups

- Tag/release is owned by the parent orchestrator after human approval of the
  Phase 10 PR — this note is documentation only until that cut.
