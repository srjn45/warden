# Schema 2 (single `warden-db`) — release notes and upgrade runbook

> **Intended for the schema-breaking major release (v11.0.0). Not yet tagged or released.**

Schema 2 moves every durable store (agents, terminals, projects/groups, plans,
pipelines, autopilot runs, context, mailbox, backend registry, usage/savings/spend,
schedules, snapshots metadata, audit/metrics, …) into **one** ScrivaDB
(`<data>/warden-db/`). This release is the first whose `schema.SchemaVersion` is
**2**: the daemon runs **only** on `warden-db`, and a schema-1 data dir is cut over
**once**, offline, by a journaled, verified migration. There is no dual-writer
mode, no silent fallback to the legacy stores, and no automatic downgrade.

## What changes for operators

| Situation | Behaviour |
| --- | --- |
| Fresh install | Stamped at schema 2 on first boot; only `warden-db/` is created. |
| Existing install, `wd update` from a release that knows the cutover | Target preflight verifies live data (daemon ownership and expected live index lag reported as non-blocking notes; genuine corruption blocks before changes) → daemon stopped → data snapshot → binary swap → the **new** binary runs `migrate --apply` (strict post-stop verification under ownership lock; explicit quarantine of malformed savings JSONL rows) → daemon restarted → must report `schema_version 2` and the new version; any failure restores previous binary **and** data without mutation. |
| Existing install, manual binary swap | The new daemon refuses to start (`must be cut over to the single warden-db`) without opening any store. Run `warden migrate --plan`, then `warden migrate --apply` (daemon stopped). |
| Older (schema-1) binary on schema-2 data | Refuses before opening any legacy store (`data_newer`, "no automatic downgrade"). |
| `warden-db/` beside a schema-1 ledger | Refused by every binary (`schema2_residue`): two candidate authorities, nothing is adopted or merged. |
| Legacy store directories after the cutover | **Kept, untouched, inert.** Cleanup is a separate post-adoption plan; nothing is deleted. |

## Upgrade runbook

1. **Update path.** `wd update --plan` shows the hops. A major boundary routes
   through the **last v10.x release**, which must carry the updater hook
   (`runUpdateMigrations`). Releases built before that hook cannot run the cutover:
   their updater applies a config-only `Migrate`, the new daemon refuses schema-1
   data, readiness fails and the updater **rolls back** (binary + data, verified
   healthy). With this release's `migrate --check` the old updater now stops
   **before** any change (`schema2_cutover_pending` is a non-clean finding), with the
   manual steps in step 3.
2. **Before you start.** Nothing to do for `wd update`; it takes an updater snapshot
   (`backups/pre-v<ver>-<ts>/`) **and** the cutover takes its own immutable,
   SHA-256-manifested copy (`backups/pre-schema2-<ts>/`, files `0400`, manifest last).
3. **Manual / old-updater path** (daemon stopped, new binary on `PATH`):
   ```sh
   warden daemon stop            # or: systemctl --user stop warden
   warden migrate --plan         # read-only: sources, target, backup path, blockers
   warden migrate --apply        # strict preflight → backup → stage → verify → promote → ledger
   warden version                # Data schema: 2
   # then start the new daemon (systemctl --user start warden)
   ```
4. **If it is interrupted** (power loss, kill): do **not** start any daemon (they all
   refuse: `interrupted_migration`). Run `warden migrate --resume` to continue from the
   journaled step, or `warden migrate --restore` to go back (a promoted `warden-db/`
   is moved to `backups/abandoned-schema2-<ts>/`, never deleted; the legacy stores
   and ledger are restored from the verified backup). A pure refusal before any live
   change (corrupt/ambiguous source, relationship violation) leaves the ledger exactly
   as it was — no journal to clear.
5. **After.** Verify with `warden doctor` and `GET /healthz` (`schema_version: 2`).
   Keep `backups/pre-schema2-*` and the legacy directories until you decide to clean
   up (out of scope here).

## What the cutover guarantees

* **Daemon stopped, one owner.** The data-dir ownership lock is held for the whole
  operation; a running daemon makes it refuse.
* **Active daemon preflight & live index lag.** The updater inspects the data directory
  before stopping the daemon. Ownership by the running daemon and expected live index
  lag (`index_stale_tail`, `index_stale_record`, `sidx_stale_tail`) are reported as
  non-blocking auto-recoverable notes. Genuine index damage (e.g. dangling offsets),
  unowned index lag, or unquarantineable source corruption block preflight before any
  binary swap or modification, pointing directly to the affected store without broad
  repair suggestions.
* **Safe savings JSONL quarantine.** Malformed records in `savings/ledger.jsonl` with
  valid line boundaries (<=16MiB) are preserved byte-for-byte in `<dataDir>/quarantine/savings/`
  with SHA-256 manifests, while valid events are imported into `savings_events`. Records
  exceeding the 16MiB line limit or lacking valid line boundaries safely block preflight
  with targeted error guidance; live-wins or broad repair are never used to bypass
  malformed JSONL.
* **Post-stop strict cutover & no-mutation rollback.** After the daemon is stopped,
  the new binary runs `warden migrate --apply` under the data-dir ownership lock. It
  re-verifies all stores strictly; unexpected foreign ownership or unrecoverable damage
  refuses the cutover before changes (`ErrRefusedBeforeChange`). Any migration or
  readiness failure triggers updater rollback, restoring the previous binary and data
  snapshot while moving an abandoned `warden-db/` aside without mutating legacy stores.
* **Strict preflight** (`corruption`, `ambiguity`, `duplicate_identity`,
  `stale_derived_state`, `unsupported_layout`, `unsafe_artifact`, `owned`,
  `unverifiable`): nothing is repaired, salvaged, merged or deleted to get past it.
* **Immutable backup first**, journaled in the ledger **before** any live change.
* **Staging export → equivalence proof** (record counts, canonical hashes, relationship
  checks) against the legacy sources, then **atomic promotion** (same-filesystem
  rename) and a second equivalence check on the promoted target, **then** the ledger
  moves to 2. The ledger is the commit point.
* **Active work is not touched.** Running/idle/rate-limited agents, non-terminal
  autopilot runs, terminals, worktrees, tmux sessions, transcripts, prompts, hints,
  exits, settings and session logs are copied or referenced as-is; the migration never
  marks, terminates or deletes them.
* **Updater rollback** after a promoted cutover restores the binary and the
  pre-update snapshot and **moves the promoted `warden-db/` aside** so the previous
  binary does not see two authorities; the immutable `pre-schema2-*` backup is never
  pruned and never chosen as an updater rollback snapshot.

## Data coverage (what is migrated, and where it lands)

Every row below is exercised by `internal/migrate` `TestRealisticCutover_*`, which
seeds a schema-1 dir through the **real** compat stores and compares through the
**real** schema-2 stores.

| Schema-1 source | Schema-2 collection(s) |
| --- | --- |
| `agents-db/agents`, `agents-db/closed` | `agents`, `archived_agents` (1:1, as the shared runtime reads them) |
| `terminals-db` | `terminals` |
| `projects` (projects + groups) | `projects` (+ additive `group_id`/`group_order`), `project_groups` — membership lists kept |
| `plans/plans-db` (plans, events, notes), `plans/plan-exports-db` | `plans`, `plan_events`, `plan_notes`, `plan_exports` |
| `pipelines-db` | `pipelines` (jobs stay embedded) + additive `pipeline_jobs` projection |
| `autopilots-db`, `autopilot/runs-db` | `autopilots` (identity), `autopilot_runs` (run ledger) — two collections, as the runtime uses them |
| `context`, `inbox`, `backends`, `schedules-db`, `known-prompts-db`, `snapshots-db` | `context_entries`, `messages`+`id_sequences`, `backends`/`backend_models`/`backend_roles`/`backend_settings`/`backend_quotas`/`backend_rl_cooldowns`, `schedules`, `known_prompts`, `snapshots` |
| `usage-snapshots/*.jsonl`, `quota-impact/*.jsonl`, `savings/ledger.jsonl`+`calibration.json`, `spend/spend.json`, `metrics/*.jsonl` | `usage_snapshots`, `quota_impact_fences`, `savings_events`+`savings_calibration`, `spend_records`, `metrics_samples` (flat files, under the runtime's own keys) |
| `audit.jsonl` | copied into `audit_events` (file kept; the daemon keeps **appending to both**) |
| `sessions-db`, `sessions/`, `closed/` (import-time copies) | not re-imported (would resurrect deleted agents); ids found only there are **reported** and stay in place and in the backup |
| `snapshots/*.transcript`, `prompts/`, `hints/`, `exits/`, `settings/`, `session-logs/`, `plans/<status>/` YAML, `config.yaml`, `backups/`, `quarantine/` | external: referenced or preserved in place, byte-identical |

## Known limits and follow-ups (documented, not hidden)

* **The updater that already shipped (≤ v10.0.1) cannot run the cutover.** Ship a v10.x
  waypoint carrying `runUpdateMigrations` first (it is independent of the schema
  bump), then v11.0.0. Without it users take the manual path above; nothing is lost
  either way (preflight blocks, or the updater rolls back).
* **Schema-1-shaped runtime records.** The ported stores still read membership lists
  (`project.agents/pipelines/…`, group `project_ids`, embedded `pipeline.jobs`,
  agent `child_*`). The migration carries these verbatim; the normalized-FK reshape
  of the original contract remains future work. `pipeline_jobs` and the staging
  `group_id` are additive and not read by the runtime.
* **Direct-disk CLI paths** that read legacy store directories are refused on schema 2
  instead of reading frozen data: offline `wd models`/backend registry, `warden repair
  sessions|agents|backends|all`, `wd savings --calibrate`. Use the daemon-backed
  interfaces or `warden repair warden-db`. Porting `savings --calibrate` to the daemon
  is a follow-up.
* **`plan-sync-hub`** is still served from its file hub; its envelopes are also staged
  into `plan_sync_envelopes` (not yet read by the runtime).
* **Index lies are healed, data lies are refused.** On `warden-db` a wrong primary
  index is rebuilt from the append-only segments at open (segments untouched); corrupt
  record bytes make the daemon refuse to start and change nothing.
* Leftover `.schema2-staging-*` directories from a crashed attempt are hidden scratch
  space (excluded from updater snapshots); remove them by hand once no attempt is
  journaled.
* The pre-ledger path (a data dir with no `schema.json` and legacy stores) is stamped at
  the schema-1 baseline and then refused until migrated; installs older than the
  support window must hop through a v10.x release so the boot importers run first.

## Release evidence

| Risk | Evidence |
| --- | --- |
| Realistic dataset preserved (live agents, autopilot run, plan/project/pipeline links, audit, savings, spend, metrics, usage, fences, snapshots, exports, prompts) | `TestRealisticCutover_PreservesLogicalDataset` |
| Legacy files byte-identical, external artifacts untouched | same test (`legacyBefore` / `externalBefore`) |
| Updater rollback marker never mistaken for a backup | `TestRealisticCutover_IgnoresUpdaterSnapshotMarker` |
| Interrupted → refuse → resume; crash after promotion → resume; restore reversible, re-cutover possible | `TestRealisticCutover_InterruptedAndResumed`, `…_CrashAfterPromotionResumesAndRestoreIsReversible` |
| Refusal changes no source byte, leaves ledger as found | `TestRealisticCutover_RefusalLeavesEverythingUntouched` |
| Updater: cutover then readiness on schema 2; rollback after cutover preserves `warden-db`; failed cutover rolls back; immutable backup never pruned | `internal/updater` `TestTxnSchema2*`, `TestPruneAndLatestSnapshotIgnoreSchema2Backup` |
| Updater hook runs the **target** binary once, verifies the ledger, skips fresh/at-target/pre-ledger targets, refuses interrupted journals | `internal/cli` `TestUpdateMigrations*` |
| Target preflight and boot guard agree before and after | `TestSchema2CheckAndBootGuardAgree` |
| Fresh install on schema 2 and restart | `TestFreshInstallStartsOnSchema2`, `TestInitCmd_*` |
| Old binaries / compat opener refuse schema-2 data | `internal/schema` guard tests, `TestOpenIsSharedOnlyAndNeverFallsBack` |
| Real binaries: refuse → plan → apply → serve the same fleet on schema 2, tmux untouched, apply beside a daemon refused | `internal/e2e` `TestCutoverThenSchema2DaemonServesTheSameFleet` |
| Daemon boots on `warden-db` (previously crashed on a non-file audit trail) | all `internal/e2e` tests |
| 514-record scale + downgrade refusal | `TestSchema2ReleaseAcceptance` |
| Live owner and live index lag preflight non-blocking; offline lag or genuine index damage blocks with direct diagnostics | `TestUpdaterTargetPreflight_*` |
| Safe savings JSONL quarantine preserves raw bytes and hashes; line-length limit blocks before change | `TestSavingsQuarantine_*` |
| Exact reported update path acceptance (active preflight, active agent/autopilot/project/plan state, stale derived index lag, malformed savings JSONL quarantine, post-stop strict cutover, and no-mutation rollback on failure) | `TestReleaseRegressionAcceptance_*`, `TestReleaseAcceptance_ReportedUpdatePath_*` |
| Full retirement lifecycle: upgrade, restart, repair, backup/restore, legacy inspection, retirement evaluation, safe cleanup, and continuous schema-2 operation | `TestLegacyStoreRetirement_FullLifecycle` (e2e), `TestRetirementLifecycleRegression` (migrate) |


## Legacy-store retirement, operator deprecation, and safe cleanup

With schema 2 activated, `warden-db` is the sole runtime authority. Legacy store
directories (`agents-db`, `sessions-db`, `projects`, `plans/plans-db`, `backends`,
etc.) left behind after cutover are inert, historical artifacts.

### 1. Invariants and deprecation policy

* **No runtime fallback:** The daemon operates strictly on `warden-db`. Opening
  legacy stores on a schema-2 directory (`storage.OpenLegacy`) is permanently
  refused and recorded as an adoption violation event (`legacy_open_refused`).
* **Offline repair deprecation:** Legacy repair tools (`warden repair sessions`,
  `warden repair agents`, `warden repair backends`, `warden repair all`) refuse on
  schema 2 (`errLegacyStoreInert`). Offline verification and repair of schema 2
  must be performed with `warden repair warden-db [--dry-run]`.
* **Support window commitment:** Legacy store directories and the immutable
  `backups/pre-schema2-*` cutover backup are retained for at least **90 days**
  (`DefaultSupportWindow`). Migration code (`0002-schema2-cutover`) and legacy
  readers are retained for the entire **v11.x** major release series.
* **No automated deletion:** Legacy source data is never deleted automatically—not
  during migration, not during daemon runs, and not during gate evaluation. Removal
  must be explicit, backup-gated, auditable, and recoverable.

### 2. Read-only legacy inspection (`warden inspect legacy`)

Operators can inspect retained schema-1 stores and compare them with the canonical
`warden-db` at any time during the support window:

```sh
warden inspect legacy          # human-readable terminal summary
warden inspect legacy --json   # machine-readable inspection report
```

Guarantees:
* **Strictly read-only:** Opens legacy and canonical stores only via private temporary
  copies. Verified by a before/after filesystem fingerprint of the data directory.
* **No dual authority:** Refuses on any data directory that is not at schema 2
  (`ErrLegacyLiveAuthority`), ensuring it cannot be used as an unintended authority.
* **Key comparison:** Lists per-collection record counts and diffs record keys
  between retained legacy stores and `warden-db`. Canonical-only records are
  expected from runtime activity post-cutover.
* **Backup re-verification:** Verifies the cutover backup and its checksum manifest.

### 3. Retirement criteria evaluation (`warden migrate --retirement`)

Before considering legacy store cleanup, evaluate the 9 stable entry criteria:

```sh
warden migrate --retirement          # human-readable criteria summary
warden migrate --retirement --json   # machine-readable criteria evaluation
```

All 9 criteria must be satisfied for an install to be marked `eligible: true`:

| ID | Requirement |
| --- | --- |
| `schema2_active` | Ledger is schema ≥ 2 with no open migration journal |
| `warden_db_present` | The `<data>/warden-db` directory exists |
| `migration_succeeded` | Ledger history records committed cutover (`0002-schema2-cutover` or fresh install) |
| `backup_verified` | Ledger-recorded backup matches its `MANIFEST.sha256` checksums (`VerifySchema2Backup`) |
| `no_unresolved_failures` | Zero unrecovered `migration_failed` or `recovery_failed` events since cutover |
| `no_rollback` | No rollbacks or restores since cutover and no abandoned schema-2 targets |
| `zero_legacy_runtime_opens` | Zero attempts to open legacy stores on schema 2 (`legacy_open_refused`) |
| `schema2_runtime_observed` | At least 3 daemon boots observed on schema 2 (`runtime_boot` telemetry) |
| `support_window_elapsed` | Current timestamp ≥ cutover timestamp + 90 days |

Evaluating this gate deletes nothing and modifies no data files.

### 4. Safe backup-gated cleanup (`warden migrate --cleanup-legacy`)

Once `warden migrate --retirement` reports `eligible: true` and the operator has
confirmed backups are stored off-host, legacy stores may be safely cleaned up:

```sh
# Step 1: Preview cleanup actions without moving anything
warden migrate --cleanup-legacy --dry-run

# Step 2: Execute backup-gated cleanup (prompts for confirmation, or pass --yes)
warden migrate --cleanup-legacy --yes
```

How cleanup executes safely:
1. **Re-runs the retirement gate:** Any unmet criterion (e.g. support window not
   elapsed, unverified backup) immediately aborts before any filesystem change.
2. **Verifies canonical authority:** Verifies `warden-db` exists and has zero
   blocking findings (`wardendb.Verify`), and re-verifies `backups/pre-schema2-*`
   against its SHA-256 manifest.
3. **Enumerates candidates:** Identifies exact migrated store paths (`agents-db`,
   `closed`, `sessions-db`, `sessions`, `terminals-db`, `projects`, `plans/plans-db`,
   `plans/plan-exports-db`, `pipelines-db`, `pipelines`, `autopilots-db`, `autopilot`,
   `context`, `inbox`, `backends`, `usage-snapshots`, `quota-impact`, `savings`,
   `spend`, `schedules-db`, `known-prompts-db`, `metrics`).
4. **Preserves external content:** Directories that contain external artifacts not
   stored in `warden-db` (such as `snapshots/*.transcript`, `session-logs/`, etc.)
   are listed as "kept" and are never moved.
5. **Confirmation gated:** Interactive `[y/N]` prompt, or explicit `--yes` flag.
   Non-interactive runs without `--yes` refuse without changes.
6. **Quarantined, not deleted:** Migrated paths are moved to
   `<data>/backups/legacy-quarantine-<UTC-timestamp>/<relative-path>`.
7. **Audit logging:** Records a `legacy_cleanup` event in `audit.jsonl` (with actor,
   quarantine path, and moved file list) and an event in `.adoption-evidence.jsonl`.
8. **Idempotent:** Re-running when no legacy stores remain is a safe no-op.
9. **Uninterrupted operation:** Schema-2 daemon operations continue seamlessly on
   `warden-db` after cleanup.

### 5. Recovery runbook

If legacy store data is needed after cleanup:
* **Immediate recovery (before quarantine removal):** Move directories back from
  `<data>/backups/legacy-quarantine-<timestamp>/` to `<data>/`.
* **Disaster recovery (from cutover backup):** The immutable cutover backup
  `<data>/backups/pre-schema2-<timestamp>/` contains the byte-identical pre-cutover
  state with its `MANIFEST.sha256`. Copy desired stores back or use documented
  backup restoration procedures.
* **Quarantine deletion:** Once operators are confident in their schema-2 operation
  and verified off-host backups, the quarantine directory under `backups/legacy-quarantine-*`
  can be deleted manually.

