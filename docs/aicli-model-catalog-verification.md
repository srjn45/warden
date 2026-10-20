# AI CLI model catalog verification matrix

This matrix records evidence for the catalog-refresh feature. Fixture evidence
is deterministic and offline; it is not evidence that an installed provider is
currently live. No daemon boot, background tick, or ordinary cached launch runs
scheduled discovery or an LLM call. A live smoke check is an operator-authorized
`aicli models discover` action only.

| Contract area | Evidence | Status / limit |
| --- | --- | --- |
| Offline, stale, malformed, partial and capped output | `modelcatalog_adapters_test.go`; 4 MiB cap, timeout classification and partial-result handling | Fixture-proven. Provider output is discarded after parsing. |
| Paginated/provider registries | Crush configured-provider filtering | Partial only; no adapter claims a complete dynamic provider registry. |
| Hidden/special models, cache identity drift, reappearance | adapter and `catalog_reconcile_test.go` | Fixture-proven. Hidden is never auto-assigned. |
| Account/profile switch between observation and apply | scope fingerprint and proposal CAS in `catalog_reconcile.go` | Fixture-proven by scope-specific observations; account references are never persisted. |
| Shared quota windows and exhaustion | existing `backendusage` fixture suites and resolver tests | Fixture-proven; catalog discovery itself does not make usage calls. |
| Pins, seed reinitialization, downgrade/rollback | model policy, seeder and reconciliation tests | Fixture-proven. Explicit pins remain an operator choice. |
| Unsupported providers | adapter capability declarations | Explicitly unsupported/non-dynamic: Claude, Aider and Goose. They remain display/config surfaces, not live catalog sources. |
| Concurrent refresh/apply and stale proposal | `catalog_reconcile_test.go` | Store mutex + proposal/head/pre-hash CAS; stale side loses without partial policy mutation. |
| Crash mid-apply | Scriva sync-mode and transaction test harness | Storage-level atomicity is covered; this feature does not claim provider-side atomicity. |
| Slow provider responsiveness | `TestSlowCatalogProviderDoesNotBlockCatalogStoreReaders`; discovery runs before `backendstore.Store.mu` | Deterministic fixture proves a blocked adapter does not block a catalog reader. `TestSlowCatalogProviderDoesNotBlockSessionsApprovalsOrTree` (`internal/daemon`) blocks a registered adapter mid-discovery and proves `/sessions`, `/tree` and an approval complete while it is still blocked. |
| Spawn/process leaks, cancellation, output caps | catalog runner tests and lifecycle recovery tests | Output cap and child reaping fixture-proven; cancellation is bounded by the command context. |
| CLI updates/profile switch and cache schema drift | version/identity/stale-cache adapter tests | Drift degrades to partial/failed; it never authorizes retirement. |
| No background discovery | daemon has no catalog scheduler or adapter wiring | `TestDaemonBootAndTicksNeverRunCatalogDiscovery` (`internal/daemon`: real `ListenAndServe` loop, background ticks, reads) and `TestCachedLaunchResolutionNeverRunsDiscovery` (`internal/lifecycle`) wire every registered backend to a discovery tripwire; none fires. |
| Startup retry, fallback and exact pins | launch-evidence and router eligibility tests | Scope-local rejection is fixture-proven. `TestStartupResolutionExactPinNeverSubstitutedAndExhaustionFailsTyped` proves an exact pin is returned unchanged even when launch-rejected, and that an all-rejected tier fails with `router.ErrAllExhausted` instead of a default model or another tier. **Deferred by decision:** automatic relaunch recovery (≤3 attempts, `ExcludeRoutes`, `model_launch_attempt` events) is not implemented; it waits for the PTY migration and state/prompt/error detection redesign (`docs/FUTURE_ENHANCEMENTS.md` #60). Startup fails fast on a single resolution, and the operator recovers with `warden models refresh --apply --yes` then `warden switch`. |

## Bounded telemetry and audit

`backendstore.CatalogMetrics` has only bounded labels: known AI CLI, completeness,
cache freshness, and classified error. It reports refresh totals, discovered,
unavailable and unknown transitions, partial results, timeouts, reconciliation
conflicts, and a fixed-bucket duration histogram. It deliberately excludes model
ids, account identifiers, profiles, prompts, raw command output and credentials.

The append-only audit records `model_catalog_refresh`, `model_catalog_apply`, and
`model_catalog_rollback`. They carry proposal/revision/snapshot provenance, a
redacted scope fingerprint, bounded operation/rationale detail, and never raw
account references or provider output. Snapshot retention is bounded in the
store; audit consumers must retain only the configured operational window.

## Release gate

Run `wd check` before release. The operator sequence is tested as preview →
inspect proposal/blocked entries → explicit apply by exact id → history → new
session routing → revision-safe rollback. Do not use a fixture as proof that a
provider is currently reachable. Any opt-in live check must identify itself as
live evidence in its operator record; CI uses fixtures only. The release gate
does not authorize a deployment, tag, local configuration change, login, paid
probe, CLI update, or restart.
