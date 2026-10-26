# Schema-3 cutover implementation checkpoint

**Incomplete: not a deployable cutover and not a task approval request.**

Bound assignment: Plan `plan-4a55e219`, revision 3, hash
`sha256:8c4f9b7a4f96e707b02722c8bc28c546eab0153c4efdb1f5b9f678cfe9da2ea0`,
task `cutover`, parent `orchestrator-8bed8d7b`, worker `worker-a91c7627`.
Integration base is `worker-8550efb9`; approved migration head
`7a9eadb7ac2947553eb0baff95f6b8f43110d0e6` was verified as an ancestor after
`warden git sync --base worker-8550efb9`.

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

Read-only GitHub release enumeration on 2026-10-26 showed
[`v11.7.0`](https://github.com/srjn45/warden/releases/tag/v11.7.0), published
2026-10-25T13:39:23Z, as the newest published release. Its tagged updater source
at `eb2bfc4e87515699f9a8ca9974bee9c93f5fadc8` has no schema-3 transaction protocol.
This is not a compatibility-tested updater
waypoint. No waypoint version was invented, release published, tag created or
fleet update performed.

`.goreleaser.yaml` runs `cmd/genmanifest` with only `-version` and `-out`.
The generator's support-window default cannot establish a compatible waypoint;
schema-3 generation remains explicitly blocked. A real schema-2 updater waypoint
must implement and pass the positive old/new compatibility contract, be published
first, and supply the schema-3 release's explicit minimum version and capabilities.
The release workflow currently triggers on tags and publishes non-draft releases;
running it is outside this assignment's authorization.

`wd check schema3-binary-boundary` builds isolated executables from the real
`v11.7.0` tag and this worktree, verifies their version/schema metadata, and tests
that they refuse schema-3 startup before opening the fixture database. This is
**negative boundary evidence**, not proof of a working schema-3 target or a
published compatible waypoint. Ordinary package tests skip these explicit builds.

## Acceptance scope still outstanding

The format constants and runtime registrations remain schema 2. Full schema-3
production codecs, atomic child-authority reads/writes, ownership validators on
every mutation/import/CAS path, consolidated autopilot runtime authority and
reserved-context retirement are not implemented by this checkpoint. Nor are
the removal of inverse helpers/bootstrap repair, response/tree adapters, or
schema-3 fresh-install activation. Do not remove incomplete-cutover guards.

The real update path still does not invoke the private schema-3 transaction.
Actual free-space/config inventory, release manifest and binary capability binding,
authenticated target readiness/lock handoff, real target start/stop and positive
old/new update/repeat/rollback acceptance remain required. Rollback fixtures use
isolated scripted service controllers; they must not be represented as real
supervisor or real schema-3 runtime acceptance.

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
