---
title: Plans migration playbooks
description: Exact operator playbooks for DB-native Plans, legacy import, replica PRs, backup restore, and export conflicts.
---

import { Aside, Steps } from '@astrojs/starlight/components';

Canonical Plans live in ScrivaDB. Repository `plans/**/*.yaml` files are optional inert replicas. These playbooks are the supported operator paths after the ScrivaDB cutover ([design freeze](https://github.com/srjn45/warden/blob/main/docs/specs/2026-09-30-scrivadb-canonical-plans.md)).

<Aside type="caution">
`wd plan scan`, `wd plan import`, and `wd plan status` are **deprecated migration aids for one release**. After a Plan is imported (or created DB-natively), they cannot affect canonical definition, lifecycle, or execution. Prefer the commands in each playbook below.
</Aside>

## 1. Fresh DB-native use

No `plans/` directory required.

<Steps>

1. Register the project if needed (`wd projects open-local .`).
2. Create a Plan:

   ```bash
   wd plan create --name feature-x --goal "ship it" \
     --task t1:implement --task t2:review
   ```

3. Inspect and run:

   ```bash
   wd plan list
   wd plan show <plan-id>
   wd plan run <plan-id> --mode autopilot   # or pipeline | orchestrator | manual
   ```

4. Complete or archive when appropriate (`wd plan complete` / `wd plan archive`).

</Steps>

## 2. Importing a legacy repository

Use this once per project that still has `plans/{pending,in_progress,completed,archived}/*.yaml` (or flat `plans/*.yaml`).

<Steps>

1. Clone or open the repo and ensure the daemon knows the project.
2. Classify without writing:

   ```bash
   wd plan import-legacy --report --json
   ```

3. Import into ScrivaDB (source files left untouched):

   ```bash
   wd plan import-legacy
   ```

4. Verify:

   ```bash
   wd plan list
   wd plan show <plan-id>
   ```

5. Matching content hash on re-import is a no-op (skipped). Differing hash → `conflicted` with no mutation — resolve by editing the canonical Plan (`wd plan` update paths) or restoring from backup, not by re-scanning.

</Steps>

<Aside>
Do **not** rely on daemon startup or `wd plan scan` to re-authorize Status from directories. Startup scan is retired.
</Aside>

## 3. Optionally publishing a replica PR

Publish an inert YAML projection for human review. Never required for create/run.

<Steps>

1. Ensure the Plan revision you want is in ScrivaDB (`wd plan show <id>`).
2. Export onto a dedicated branch and open/reuse a PR:

   ```bash
   wd plan sync_to_repo <plan-id> --base <integration-or-main>
   # optional path override:
   wd plan sync_to_repo <plan-id> --base main --path plans/pending/feature-x.yaml
   ```

3. Review the PR. Editing the exported YAML does **not** change listing or execution.
4. Repeating the same revision/hash for the same repo/ref/path returns the prior sync result (no duplicate PR spam).

</Steps>

## 4. Recovering from a backup

Git alone cannot rebuild execution evidence. Use a Plan backup bundle.

<Steps>

1. On the source:

   ```bash
   wd plan backup export --all -o plans-backup.json
   ```

2. Transfer the bundle to the destination (not via `plans/` YAML alone).
3. On the destination:

   ```bash
   wd plan backup restore plans-backup.json --dry-run
   wd plan backup restore plans-backup.json --on-conflict skip
   wd plan list
   wd plan show <plan-id>
   wd plan run <plan-id> --mode manual
   ```

</Steps>

Full cadence and conflict policy: [Plan backup and restore](/warden/guides/plan-backup-restore/).

## 5. Resolving an export conflict

`sync_to_repo` refuses to overwrite a non-Warden file at the target path on the export branch.

<Steps>

1. Read the structured conflict from the CLI/API/MCP response (`outcome=conflict` / error message naming the path).
2. Choose one:
   - **Different path** — re-run with `--path plans/pending/<other-name>.yaml` (or another free path under `plans/`).
   - **Keep foreign file** — leave the canonical Plan unchanged; do not force-overwrite.
   - **Replace after review** — manually resolve on the export branch outside warden, or delete/move the conflicting file with an explicit operator commit, then re-run `sync_to_repo`.
3. Confirm the ScrivaDB Plan is intact: `wd plan show <plan-id>` (revision/hash unchanged by a failed sync).

</Steps>

<Aside type="tip">
Import conflicts (legacy YAML vs existing canonical hash) are separate: `import-legacy` reports `conflicted` and never clobbers. Fix the canonical record deliberately, or use backup restore with an explicit `--on-conflict` policy.
</Aside>
