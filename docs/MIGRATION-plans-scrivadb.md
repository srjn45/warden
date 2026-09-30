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
| `wd plan scan` reseeds Status from dirs | Deprecated: stubs only; **no Status reseed** for Plans with a non-empty definition |
| `wd plan import` → pending + scan | Deprecated; prefer `import-legacy` / `create` |
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
3. **Optional replica PR** — `wd plan sync_to_repo`.
4. **Backup / machine transfer** — `wd plan backup export|restore`.
5. **Export path conflict** — choose another `--path` or resolve foreign file; canonical Plan stays intact.

Exact steps: site guide `guides/plans-migration`.

## Operator checklist

- [ ] Stop treating directory placement as lifecycle authority
- [ ] Run `import-legacy` once per legacy project (not on every daemon start)
- [ ] Replace scripts that call `plan scan` with `import-legacy` or DB-native CRUD
- [ ] Prefer backup restore over `git pull` + scan for recovery
- [ ] Expect scan/import/status removal in a subsequent release
