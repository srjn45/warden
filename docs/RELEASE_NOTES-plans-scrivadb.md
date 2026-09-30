# Release notes — ScrivaDB-canonical Plans (compatibility retirement)

**Phase 11** of `scrivadb-canonical-plans-repo-sync` (design freeze:
`docs/specs/2026-09-30-scrivadb-canonical-plans.md`).

## Highlights

- ScrivaDB is the sole Plan authority for definition, lifecycle, and execution.
- Daemon startup no longer scans `plans/` (already retired; remains a no-op).
- `wd plan scan` / `import` / `status` (and MCP/API equivalents) are **deprecated
  for one release**. Responses include an explicit notice that they **cannot
  affect canonical execution after import**. Scan does not reseed `Status` for
  Plans that already have a non-empty definition (`skipped_canonical` count).
- Operator playbooks: fresh DB-native use, legacy import, optional replica PR,
  backup restore, export conflict — see site `guides/plans-migration` and
  `docs/MIGRATION-plans-scrivadb.md`.

## Surfaces updated

OpenAPI (`deprecated: true` on scan + legacy project PATCH), generated client,
CLI help, MCP tool descriptions, `docs/FEATURES.md`, root feature catalog,
`docs/USAGE.md`, site concepts/guides, skill, README What's new.

## Follow-ups

- Remove deprecated scan/import/status surfaces in a subsequent release.
- JSON export format and Hub network transport remain deferred.
