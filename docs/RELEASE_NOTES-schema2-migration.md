# Schema-2 migration release validation

Schema 2 is an offline, journaled cutover from the schema-1 store layout to
`warden-db/`. The daemon does not migrate data at startup.

## Operator runbook

1. Stop the daemon and take the normal operator backup.
2. Run `warden migrate --plan` and resolve every blocking finding. A corrupt
   source is intentionally left untouched; repair or restore it before retrying.
3. Run `warden migrate --apply`. The migration creates and verifies its own
   immutable pre-schema-2 backup before it can promote the target.
4. If the command or host is interrupted, do not start the daemon. Run
   `warden migrate --resume` to continue the recorded phase, or
   `warden migrate --restore` to restore the verified pre-migration snapshot.
   Restore preserves a partially promoted `warden-db/` under `backups/` for
   forensics instead of deleting it.
5. Start a schema-2-capable binary only after apply or resume completes. A
   schema-1 binary refuses a schema-2 data directory; there is no automatic
   downgrade.

## Release acceptance evidence

The focused migration suite supplies the release evidence below:

| Risk | Evidence |
| --- | --- |
| Interrupted phases and resume | `TestRunner_CrashAndResumeAtEachStep` |
| Restore from an interrupted journal | `TestRunner_Restore` |
| Immutable backup corruption detection | `TestVerifySchema2Backup_Detects` |
| Corrupt legacy source is actionable and unchanged | `TestPlanSchema2_Blocks`, `TestV925ToV928FailurePreflightCatchesWithZeroDowntime` |
| Corrupt or divergent target is rejected | `TestVerifySchema2Equivalence_*` |
| End-to-end cutover, restart, downgrade refusal, and large input | `TestSchema2ReleaseAcceptance` |

`TestSchema2ReleaseAcceptance` migrates 514 legacy agent records, proves a
schema-2-capable restart needs no further migration, asserts a schema-1 binary
receives the `data_newer` refusal, and verifies the promoted target's canonical
equivalence.
