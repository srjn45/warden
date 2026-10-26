# Schema-3 cutover implementation checkpoint

**Incomplete: not a deployable cutover and not a task approval request.**

Bound assignment: Plan `plan-4a55e219`, revision 3, hash
`sha256:8c4f9b7a4f96e707b02722c8bc28c546eab0153c4efdb1f5b9f678cfe9da2ea0`,
task `cutover`, parent `orchestrator-8bed8d7b`, worker `worker-a91c7627`.
Integration base is `main`; the approved schema-2 waypoint is released
`v12.1.0` at `5fceb280`.

## Changes in this checkpoint

* Readiness finalization resumes across every intent/completion and ledger-save
  boundary. A finalized ledger with durable readiness intent is validated without
  rerunning migration or recreating its pending marker. Finalization binds all
  ledger transaction fields, not merely the backup directory. Finalized retries
  permit valid subsequent writes while checking ownership, checksums and indexes.
* Restore-selected transactions cannot be promoted or finalized again. A repeated
  completed restore is a no-op only while the paired restored generation, config
  and binary remain unchanged; later old-daemon writes are never overwritten by
  replaying the old authorization. Restore resume validates the manifest/journal
  target identity without requiring the target executable to remain installed.
  Empty and unterminated journals refuse recovery.
* The public rollback adapter accepts an exact `--transaction` and requires
  `--accept-data-loss` independently of `--yes`. It verifies a sealed paired
  manifest before stopping a service, proves stopped managed service and absent
  health before acquiring ownership, rechecks the supervisor/health after taking
  ownership, and restores the full selected binary/data generation. It releases
  updater ownership before starting the old service and verifies its exact old
  version and schema. Failed old readiness stops the service. Generic snapshot
  and `.bak` selection refuse schema-3 data or a bound pending transaction.
* `storage.Open` requires an exact compatible committed ledger for an existing
  DB. It no longer registers schema-2 codecs on a newer generation or adopts a
  ledger-less existing DB. Only a proven fresh install is stamped. Schema-3 guard
  guidance points to updater/paired rollback authority rather than unsupported
  standalone migration mutations.
* Authentic whole-Pipeline and Plan wire fixtures exposed a mapper gap:
  schema-2 `Pipeline.MarshalJSON`, used by persistence, includes computed
  `job_count` and `jobs_done`. These known derived counters are now checked against
  the embedded jobs and retired from target headers. Contradictory counters block;
  unknown fields and checksum/numeric-precision refusals remain unchanged.
* CI branch filters include stacked worker/docs PR targets. The PR can retain its
  approved integration base while receiving the ordinary CI gates.

## Release evidence and unresolved input

The approved updater waypoint is now the released `v12.1.0`, commit
`5fceb280`, running schema 2. The schema-3 boundary test archives that exact
commit into an isolated temporary tree; it never reads or changes a live data
directory or daemon.

`.goreleaser.yaml` runs `cmd/genmanifest` with only `-version` and `-out`.
The generator's support-window default cannot establish a compatible waypoint;
schema-3 generation remains explicitly blocked. A real schema-2 updater waypoint
must implement and pass the positive old/new compatibility contract, be published
first, and supply the schema-3 release's explicit minimum version and capabilities.
The release workflow currently triggers on tags and publishes non-draft releases;
running it is outside this assignment's authorization.

`wd check schema3-binary-boundary` builds isolated executables from that release
and this worktree. It verifies the old binary refuses schema-3 before opening a
fixture. Positive target-start/readiness and restored-old-daemon proof remain
outstanding: the current pending-ledger boot guard rejects the target before it
can authenticate its transaction. Ordinary package tests skip these explicit
builds.

## Acceptance scope still outstanding

The format constants and runtime registrations are schema 3. Normalized
production codecs own agents, projects, pipelines/jobs, plans/tasks and
autopilot runs; the retired inverse stores are not registered in schema-3 mode.
Fresh installs stamp schema 3. Historical schema-2 tests must continue to test
the schema-2 migration stage explicitly rather than treating it as a daemon-
startable current format.

The update path invokes the private schema-3 transaction, including capability
and manifest binding, paired backup, lock handoff and authenticated target
readiness. The acceptance test uses a real target daemon only inside its isolated
fixture; rollback's supervisor seam remains deliberately scripted.

Focused tests cover readiness crash boundaries and ledger identity tampering,
post-update writes, full paired restore despite unrelated snapshots/`.bak`, absent
data-loss consent, stop failure, still-answering health, wrong old version/schema,
interrupted binary replacement, restore replay after later writes, complete
Pipeline/Plan payloads and conflicting derived counters. Required checks and CI
results must accompany the exact pushed head in the coordinator handoff.

All changes and tests use isolated fixtures. No real user data was inspected or
migrated; the fleet daemon was not stopped; no standalone migration apply ran.
Keep the stacked PR open and unmerged. Plan/task progress remains in progress;
no `job done`, task completion, auto-merge or landing is authorized.

## Updater wiring (waypoint layer)

The updater now drives the format-3 transaction from `warden update`; after the
paired promotion the runtime must serve schema 3. A format-3 target is accepted only when all of these
agree: a validated release manifest (`min_schema=2`, `breaking`, exact
`Schema3Requirements`, `min_upgrade_from` ≤ the running updater), the staged
binary's own `version --json` (`version`, `schema_version=3`,
`updater_capabilities`), the running binary's schema (2) and measured free space.
Flow: stop → exclusive ownership → verified paired backup (data, ledger, config
inside the data dir, exact old binary) → swap → config reconcile → promote →
owner-lock handoff → restart → exact version/format readiness → target-side
`migrate --verify-ownership` → finalize. Any failure after the backup restores
the paired binary and data and proves the old daemon healthy. The transaction id
is printed for `warden rollback --transaction`. A schema-2 update never reaches
this path.

The target-side verification command is implemented and deliberately read-only.
It binds the started executable hash, sealed paired manifest, append-only
transaction journal, pending ledger intent, and normalized target graph to the
exact transaction before allowing the updater to finalize readiness. It refuses
missing or mixed migration actions and cannot grant migration authority to an
ordinary CLI invocation.
