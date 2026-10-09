# Update process redesign — versioned data, planned upgrade paths, data-safe rollback

Status: DRAFT for review (2026-10-09). No code yet.

## 1. Why (the incident that prompted this)

`wd update v9.25.0 → v9.28.0` rolled back four times. v9.28.0 opens every
ScrivaDB store with integrity policy `fail`; v9.25.0 did not. Latent
revision-regression conflicts and duplicate live keys in `context`, `backends`,
`agents-db/closed`, `inbox/messages`, `plans/*` and `projects` therefore turned
into boot failures. Fixing it took a manual rebuild of seven collections.

Root causes in the current process:

1. **Preflight covers one store.** `backendPreflight` checks `backends/` only,
   and with the *old* binary's strictness. The new binary's strictness is what
   matters.
2. **No data-format version.** Migrations are implicit: boot-time importers
   guarded by sentinel files (`.sessions-filedb-imported`, ...) plus config
   migrations. Nothing records "this data dir is at format N", so the updater
   cannot know whether an update needs migration, in what order, or whether a
   jump is supported.
3. **Rollback restores the binary, not the data.** If the new version mutated
   data (migration, index rebuild) the old binary may not open it.
4. **No supported-upgrade policy.** v1→v5 is "swap the binary and hope".
5. **Old writers produced bad history.** Fixing the updater does not remove the
   need to stop creating conflicts.

## 2. Principles

- The updater plans before it acts; `wd update --plan` changes nothing.
- All checks run with the **target** binary against a **copy-safe** view of the
  data, before the running daemon is touched.
- Migrations are an ordered, idempotent, journaled sequence run only by the
  updater / `warden migrate`. The daemon never runs a destructive migration at
  boot.
- Every update that touches data takes a data snapshot first; rollback restores
  binary **and** data.
- Running agents are never affected (tmux sessions are not touched).
- Fresh install and upgrade share one code path (`install.sh` → `warden init`
  = migrate from schema 0).

## 3. Data-format version (schema ledger)

`<data>/schema.json`:

```json
{ "schema_version": 14,
  "binary_version": "9.28.0",
  "history": [ {"from": 13, "to": 14, "migration": "0014-scriva-integrity-strict",
                "at": "...", "backup": "backups/pre-v9.28.0-20261009T163000Z"} ],
  "in_progress": null }
```

- `schema_version` is a monotonically increasing integer, **separate from
  semver**. Many releases share one value; it bumps only when stored data or
  config format changes.
- Each binary embeds `SchemaVersion` (what it writes) and `MinSchema` (oldest
  it can migrate from).
- `in_progress` is the journal: `{migration, step, snapshot}` written before
  each migration, cleared after. A crash mid-migration is resumable or
  restorable, never silent.
- Legacy installs with no ledger are detected by their sentinel files and
  stamped with an inferred baseline version once; the sentinels are then
  retired.

Daemon boot guard:

| data schema vs binary          | behaviour                                          |
|--------------------------------|----------------------------------------------------|
| equal                          | start                                              |
| data newer than binary         | refuse: "data written by newer warden; upgrade"    |
| data older, within MinSchema   | refuse: "run `warden migrate` (or `wd update`)"    |
| data older, below MinSchema    | refuse: "unsupported direct upgrade, see path"     |
| `in_progress` set              | refuse: "interrupted migration; `warden migrate --resume` / `--restore`" |

Only trivially safe, non-lossy maintenance (rebuilding a derived index) may
still happen at boot.

## 4. Migration registry

```go
type Migration struct {
    ID       string            // "0014-scriva-integrity-strict"
    From, To int               // schema versions
    Kind     Kind              // Schema | Data | Config
    Check    func(Env) []Finding   // read-only: does this data need/permit it?
    Run      func(Env) error       // idempotent
    Verify   func(Env) error       // post-condition, run before commit
}
```

Rules: idempotent, re-runnable after a crash, no dependence on network, each
step verified before the ledger advances. Existing importers and
`internal/autopilot/migrate.go` are ported into the registry.

## 5. Release manifest and upgrade paths

Each release publishes `manifest.json` (checksummed alongside the archives,
produced by GoReleaser):

```json
{ "version": "9.28.0", "schema_version": 14, "min_schema": 11,
  "min_upgrade_from": "9.20.0",
  "waypoint": false,
  "breaking": false,
  "notes": ["strict integrity on open"],
  "requires": {"manual": []} }
```

- `min_upgrade_from`: oldest version that may upgrade **directly** to this one.
- **Waypoints** are releases that carry the migrations bridging a compatibility
  window and are guaranteed to run on data from the previous window. Policy:
  a waypoint at every breaking change and at least every N minors.
- Path planning: given installed version A and target T, walk
  `min_upgrade_from` back until A is covered. Example v1 → v5 with waypoints
  v2, v3, v5: the updater downloads v2, v3 and v5 (all checksum-verified),
  runs `v2 migrate`, then `v3 migrate`, then `v5 migrate`, then installs v5.
  Each hop's binary only needs to know migrations of its own window, so old
  migration code can be pruned at the next waypoint instead of being carried
  forever.
- Downgrade across a schema bump is refused unless via `wd rollback` (restores
  the pre-update snapshot).

## 6. The update transaction (extends `internal/updater/transaction.go`)

```
 plan → download+verify → preflight(target binary) → confirm
      → stop daemon → snapshot data → migrate chain (journaled)
      → swap binary → start → readiness (version AND schema match)
      → commit ledger → prune old snapshots
 any failure after "stop daemon": restore snapshot + old binary, restart, verify.
```

1. **Plan** (`wd update --plan`, also the first step of a real run): resolve
   target, fetch manifests, compute the path, list migrations, breaking
   changes, expected downtime, free disk needed, whether a manual step is
   required. Real runs stop for confirmation when `breaking` or a data
   migration is involved (`--yes` for scripts).
2. **Download + verify** every binary on the path into staging. Nothing live is
   touched.
3. **Preflight with the target binary:** `warden-new migrate --check
   --data-dir <live>` opens a verification of **every** store using the target
   engine's strictness, read-only. Findings are classified: *auto* (migration
   fixes it), *repairable* (`warden repair ...` suggestion), *blocking*. A
   blocker aborts before anything is stopped, with the exact command to run.
   This would have caught today's failure in seconds with zero downtime.
4. **Stop** the daemon cleanly (tmux sessions untouched). The ownership lock
   guarantees no writers.
5. **Snapshot** the stores (not transcripts) to
   `<data>/backups/pre-<ver>-<ts>/` using hard links / reflink where possible,
   verified by checksum; recorded in the ledger journal.
6. **Migrate** the chain. Each step: write journal → `Run` → `Verify` → advance
   ledger. Any failure triggers restore from the snapshot.
7. **Swap + start + readiness.** `/healthz` reports `version` and
   `schema_version`; both must match the target. Today only version is checked.
8. **Commit** and keep the snapshot for a retention window (default: last 2 or
   14 days), then prune.

Failure matrix: failure before step 4 changes nothing; after step 4 restores
data + binary and verifies the old daemon is healthy; if restore itself fails
the output states exactly what to run by hand.

## 7. `wd rollback`

Restores the most recent snapshot and the previous binary (`.bak`). If no
schema change happened it is a plain binary swap. Otherwise it warns that
changes made since the update are lost, and requires confirmation.

## 8. Repair: make the manual fix a supported command

Today `warden repair backends|agents` deliberately refuse ambiguous history, so
the only way out was a hand-written rebuild. Add an explicit, backup-first
policy: `warden repair all --resolve-history=live-wins` which, per collection,
rebuilds from the live (open-resolved, last-line-wins) records, keeps the
newest record per `_key`, writes a report of every dropped record, quarantines
the originals, and verifies. It is never invoked implicitly; preflight prints
it as the suggested command. (Prefer upstreaming the rebuild as a ScrivaDB
`Repair` mode.)

## 9. Stop producing bad history

Find and fix the writers that logged lower revisions after higher ones and that
allowed two live records per key (autopilot state keys, plan events, inbox
messages). Add an invariant check in CI.

## 10. Testing — the part that makes this trustworthy

- **Golden upgrade fixtures:** keep a data dir produced by each waypoint
  release (and by representative past versions). CI job: for every fixture,
  run the real updater against the current build, assert clean migration,
  verify, daemon healthy, and rollback restores byte-identical data.
- Fault injection at each transaction step (kill mid-migration, disk full,
  health check fails) using the existing fakes in `internal/updater`.
- A multi-hop test (oldest supported → latest) as a release gate.

## 11. Install parity

`install.sh` stays the same shape (download, verify, install, default config)
and then runs `warden init`, which creates the data dir by running the
migration chain from schema 0 and writes the ledger. Upgrade and fresh install
therefore cannot drift.

## 12. Rollout plan (one PR each, in order)

1. Schema ledger + `schema_version` in `/healthz` + boot guard (+ legacy
   baseline detection).
2. Migration registry; port existing importers and autopilot migration.
3. `warden migrate --check/--apply/--resume/--restore` (target-binary
   whole-data preflight).
4. Data snapshot + data-aware rollback in the transaction; `wd rollback`.
5. Release manifest in GoReleaser; path planner; `wd update --plan`.
6. `warden repair all --resolve-history=live-wins`.
7. Fix bad-history writers (§9).
8. Upgrade fixtures + fault-injection CI (§10).
9. Docs: README, `docs/`, website guide + generated CLI reference, skill.

## 13. Open decisions

1. **Waypoint hops vs. one binary carrying all migrations.** Recommended:
   waypoints (bounded code, each hop independently tested). Cost: updater
   downloads several binaries on a long jump.
2. **Boot-time auto-migrate vs. update-only.** Recommended: update/`warden
   migrate` only; daemon refuses with a clear message.
3. **Support window.** Recommended: direct upgrade from the last 3 minors,
   waypoint at every breaking change.
4. **Snapshot retention.** Recommended: keep last 2 or 14 days.
5. **Connected clients** (hub, Android app): include an API-version
   compatibility note in the manifest and have the plan warn when a breaking
   daemon API change is involved.
