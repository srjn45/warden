# Release notes — File-conflict detector retired (Unreleased)

Plan `plan-bb8c6f34` (revision 2). This ships in the next release; no version
or tag is assigned here.

## Highlights

- Warden no longer detects, reports, or hints about files edited by more than
  one agent. The filesystem watcher, dirty-file cache, periodic Git change
  scan, and detector-driven warnings are gone. There is **no replacement
  detector**.
- Agents still coordinate through directed messages (`send_message`,
  `read_inbox`), the shared context blackboard (`ctx_*`), and opt-in branch /
  CI tracking (`get_branch_status`, `GET /api/v1/collab/branches`).
- Explicit Git operations are unchanged: worktree creation and cleanup,
  commit / push / sync, snapshots, autopilot landing, and plan export.

## Removed surfaces

| Surface | Retired |
| --- | --- |
| Config (nested) | `collab:` block — `enabled`, `interval`, `git_reconcile_interval`, `hint` |
| Config (flat aliases) | `collab_enabled`, `collab_interval`, `collab_hint` |
| REST / OpenAPI | `GET /api/v1/collab/conflicts` (now `404`) |
| CLI | `wd workspace conflicts`, `wd workspace who-is-editing`, and the hidden `wd collab` compatibility group |
| MCP | `get_collaboration_status`, `who_is_editing_file` |
| REPL | the `get_collaboration_status` tool |
| Web | the conflicts card |
| Spawn prompt | the collaboration hint that told new agents to check for concurrent file edits |

Unknown `/api/*` paths now return `404` instead of falling through to the web
app's HTML shell.

## Upgrade guidance

- **Existing config keeps working.** The retired keys, in nested, flat, or
  mixed form (even with values that used to fail validation, such as a zero or
  unparseable `collab.interval`), are ignored on load with a one-line warning.
  Nothing else in the file is touched.
- **Cleanup is automatic.** `wd update`, daemon start, and `wd config init` rewrite the file without the retired entries, keeping every
  other setting, unknown keys, and comments. The cleanup is idempotent. You can
  also delete the entries by hand.
- **No data migration.** The detector kept its state in memory only; there is
  nothing on disk to convert, and the store schema is unaffected.
- **Agent prompts refresh on relaunch.** Already-running agents keep the old
  collaboration hint in their prompt (and a stale tool name that no longer
  exists) until they are restarted or relaunched; new spawns omit it.
- **Scripts and clients** that call the removed REST route, CLI commands, or
  MCP tools must stop; they get a `404`, an unknown-command error, or no tool.
  Use `get_branch_status` for branch / CI state and messages / context for
  coordination.

## Verification

- Update acceptance: `TestUpdateAcceptance_RetiredCollabConfigAndSchemaCutover`
  (`internal/cli`) drives the real `runUpdateMigrations` hook against isolated
  temporary config and data directories seeded with a schema-1 installation
  (live agent, autopilot run, plan, pipeline) and a config carrying nested and
  flat retired keys plus unrelated settings. It checks pre-update tolerance,
  key removal, preservation of settings and comments, the schema-2 cutover with
  live agent state intact, and a no-op second pass.
  *Simulated:* the binary swap — the target schema and migrate subprocess are
  in-process test seams; no released binary is downloaded or executed.
- Existing updater and migration acceptance (`TestUpgradeFixtures`,
  `TestRealisticCutover*`, `TestReleaseRegressionAcceptance*`) still pass.
- Behavioral absence checks cover REST, OpenAPI, CLI, and MCP; an
  active-worktree startup test asserts no detector-era Git polling.
