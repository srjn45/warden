# Migration notes — ScrivaDB-canonical Plans

**Release window:** one release of deprecated migration aids after the
`scrivadb-canonical-plans-repo-sync` cutover (Phase 11 /
`docs/specs/2026-09-30-scrivadb-canonical-plans.md`).

## What changed

| Before | After |
|---|---|
| YAML under `plans/` is definition SoT | ScrivaDB Plan record is sole authority |
| Directory placement = Status | `Plan.Status` field only |
| Daemon startup scans `plans/` | Startup scan **retired** (no-op) |
| `wd plan scan` reseeds Status from dirs | Retired (hidden alias): stubs only; **no Status reseed** for Plans with a non-empty definition |
| `wd plan import` → pending + scan | Retired (hidden alias); use `import-legacy` / `create` |
| Recovery via `git pull` + scan | Recovery via `wd plan backup restore` |

## Deprecated for one release (then removable)

- `POST /api/v1/projects/{id}/plans/scan` (`ScanProjectPlans`) — response includes `notice` + `skipped_canonical`
- `wd plan scan` / MCP `scan_plans`
- `wd plan import <file>`
- `wd plan status` / MCP `update_plan_status` / `PATCH …/projects/{id}/plans/{plan_id}` (prefer `run` / `complete` / `archive`)

Help text and API/MCP responses state that these **cannot affect canonical execution after import**.

## Supported cutover and ops

1. **Fresh DB-native** — `wd plan create` → `run` / `complete` / `archive` (no `plans/` required).
2. **Legacy repo** — `wd plan import-legacy [--report]` once.
3. **Optional replica PR** — `wd plan sync-to-repo`.
4. **Backup / machine transfer** — `wd plan backup export|restore`.
5. **Export path conflict** — choose another `--path` or resolve foreign file; canonical Plan stays intact.

Exact steps: site guide `guides/plans-migration`.

## Operator checklist

- [ ] Stop treating directory placement as lifecycle authority
- [ ] Run `import-legacy` once per legacy project (not on every daemon start)
- [ ] Prefer `wd plan create` / `run` / `complete` / `archive` for new work
- [ ] Use `wd plan backup export|restore` for machine transfer (not `git pull` + scan)
- [ ] Treat `sync-to-repo` as optional review replicas only

## Acceptance

Phase 12 upgrade gate (GREEN):
[`docs/specs/2026-09-30-scrivadb-canonical-plans-acceptance.md`](specs/2026-09-30-scrivadb-canonical-plans-acceptance.md).

Explicitly deferred (follow-up issues, not hidden non-goals): Hub network transport
([#586](https://github.com/srjn45/warden/issues/586)).

JSON export format ([#585](https://github.com/srjn45/warden/issues/585)) is **shipped**
as opt-in inert replicas (`--format json`); YAML remains the default. JSON under
`plans/` is never scan/import-legacy authority.
- [ ] Replace scripts that call `plan scan` with `import-legacy` or DB-native CRUD
- [ ] Prefer backup restore over `git pull` + scan for recovery
- [ ] Expect scan/import/status removal in a subsequent release
