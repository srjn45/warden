# Plan Finish Flow

**Date:** 2026-10-05
**Status:** Design (spec freeze for `plan-e7012db0`; t2–t6 implement it)
**Feature branch:** `autopilot/plan-finish-flow`
**Amends:** [`2026-10-05-autopilot-run-to-final-pr.md`](2026-10-05-autopilot-run-to-final-pr.md) §E.5 and §E.6 (a run no longer completes on a green final PR, and a closed final PR no longer completes it).

---

## 0. Problem

An autopilot plan is marked `completed` as soon as its final PR (integration → default branch) is **green**, not merged. Verified in the code:

- `completeRunPass` (`internal/autopilot/completion.go`) calls `completeFinal` on `GateGreen`, and also when the PR is `merged` **or `closed`**.
- The completion watcher (`internal/daemon/plan_completion.go`) then runs `Finalize`, whose only branch gate is "no open PR on `plan.Branches`". `plan.Branches` never contains the integration branch (`matchPlanBranches` skips `autopilot/*`), so nothing checks that the work reached the default branch.
- Nothing deletes the integration branch, and the plan record keeps no trace of the final PR once the executor is torn down.
- `plan archive` is a bare status flip (`Transition` → `archived`); executor, agents and worktrees stay. `canTransition` has no way out of `archived`.

Rules that hold for every section: autopilot never merges, approves or closes the final PR and never pushes to the default branch; no gh, git or model call is made while holding `c.mu`; pipeline, orchestrator and manual plans keep their completion behaviour unless a section says otherwise.

---

## 1. `awaiting_merge` run state

**Decision: as recommended.** A new *reported* run state `awaiting_merge`, derived the same way as `finalizing`: the internal state stays `StateActive`, so pause, stop and the kill switch keep working unchanged.

- **Entered** when the final PR's gate is green for its current head SHA.
- **On entry** (once): the manager and every remaining run agent (stray workers, resolvers) are terminated and their worktrees removed; the owner is notified once with the existing text, `autopilot run <id> — final PR #<n> is green and waiting for your review (autopilot will not merge it).` (the word "complete" is dropped; the run is not complete).
- The plan stays `in_progress`. The run reaches `StateComplete` **only** when the final PR is observed merged (§2), or when integration has nothing beyond the default branch (`ErrNothingToMerge`, unchanged).
- **No agent is alive only to wait.** The guardian skips manager-loss respawn, heal, overwatch nudges and the progress watchdog for a run that is awaiting merge; its `next_step` reads `check final PR #<n> for merge` with the next poll time, owner `daemon`.
- **Persistence.** `completionState` is in-memory today and is re-derived after a restart by re-asking the manager. That cannot work with no manager, so the run's persisted surface record (`surfaceRecord`, in the ledger) gains:
  `awaiting_merge: {since, green_sha, notified}` and `verified: true`. After a restart a run with `awaiting_merge` set goes straight to polling; `done_when` is not re-verified and the owner is not re-notified.
- **Leaving the state:** merged → complete (§4); regression → `finalizing` (§3); tasks appended to the plan → `active` (record cleared, the guardian respawns the manager as for any active run); pause/stop → as today (polling stops while not active).

Reported everywhere run state is shown: `wd autopilot status`, the plan `executor.state`, MCP `autopilot_status` / `get_plan`, TUI, web cockpit — label "awaiting final PR merge (#n)".

## 2. Polling

**Decision: as recommended.** Config key name: `autopilot.completion.merge_poll_interval` — joins the existing `completion` keys from run-to-final-pr §G.1 (`merge_default`, `manager_verify_timeout`). Snake_case duration string, same style as sibling keys. (Today `AutopilotConfig` has no `completion` struct yet; those keys live in `CompletionPolicy` defaults. t2 adds the YAML block and this field together.)

```yaml
autopilot:
  completion:
    merge_default: true
    manager_verify_timeout: 30m
    merge_poll_interval: 2m   # hot-reloaded; values below 30s are raised to 30s
```

- One `gh pr view <n> --json state,mergedAt,headRefOid,mergeable,mergeStateStatus` per interval per waiting run. No model call. Runs inside the existing completion tick: snapshot under `c.mu`, I/O unlocked, serialized by the existing per-run `complete:<run>` lock (`TryLock`, so at most one in flight).
- A result is discarded if, when it is applied, the run is no longer `active`, its generation changed, or the recorded final PR number differs.
- A failed poll (network, gh error) is retried at the next interval and never parks the run.
- **Restart hazard found in the code.** `EnsureFinalPR` looks only for an *open* PR. If the PR was squash-merged while the daemon was down, integration still has commits the default branch lacks, so today's code would open a **second** PR. Rule: when a final PR number is recorded, its state is read **before** `EnsureFinalPR` is called; `merged` goes to §4 and `closed` to §3 without touching `EnsureFinalPR`.

`FinalPRState` already carries `State: open | merged | closed` from `gh pr view`; GitHub reports `MERGED` for merge, squash and rebase alike, so squash detection needs no git ancestry check. t2 adds `Mergeable` / `MergeStateStatus` and `MergedAt` to it.

## 3. Regressions while waiting

| Observed | Action |
|---|---|
| Head SHA unchanged, PR open and mergeable | Nothing. The gate is **not** re-run (a local gate every 2 minutes would be expensive, and a green SHA stays green). |
| Head SHA changed (someone pushed to integration, or used "Update branch") | Leave `awaiting_merge`, report `finalizing`, re-gate the new head. Green → back to `awaiting_merge` (no second notification). Red → existing final-fix loop. |
| `mergeable: CONFLICTING` / `mergeStateStatus: DIRTY`, or `BEHIND` | Re-enter the existing completion pass from §E.2: `MergeDefault`; a clean merge is daemon-only, a conflict calls the `base_merge` resolver. Then re-gate. |
| Closed without merging | Park as needs-attention, new failure kind `final_pr_closed`: `the final PR #<n> was closed without merging. Reopen it, or run "wd plan resume" to open a new one; run "wd plan stop" to end the run and keep the branch.` |
| Merged, then reverted | **Out of scope.** The plan stays completed; a revert is a new change. |

Deviation from the recommendation, deliberate: merely being behind the default branch is **not** a regression. GitHub reports `BEHIND` only when branch protection requires an up-to-date head, which is exactly when the owner cannot merge; otherwise a behind-but-clean PR is mergeable and re-merging on every default-branch push would burn CI for nothing.

All existing bounds apply unchanged (`MaxFinalFixes`, resolver attempts, `final_pr_unfixable`). Agents are spawned only when a fix is needed (resolver for a conflict or a red gate); the manager is **not** respawned for a regression, because the landing pass lands `final-fix-*` PRs without it and `verified` is persisted.

This replaces §E.6's "merged or closed by a human ⇒ the run completes immediately": merged completes, closed parks.

## 4. After the merge

Order, all daemon-owned, no agent involved:

1. Poll observes `merged` → surface record `final_pr.state = merged`, audit `autopilot_final_pr_merged`, then `CompleteRun` through `completeFinal` (owner notified: `final PR #<n> merged — plan complete`).
2. Completion watcher (60s tick) sees `StateComplete`. **Before** tearing the executor down it copies the outcome (§6) from the run status onto the plan record.
3. `Finalize`: summary, executor/agent/worktree cleanup, status `completed` (unchanged).
4. Delete the integration branch: local `git branch -D`, then `git push --no-verify origin --delete`. Missing refs count as success.
5. Record the result on the plan (`branch_fate`, §6).

**Deletion safety.** The branch is deleted only if the merged final PR's head SHA equals the branch tip (or the tip is reachable from the default branch). Commits pushed after the merge → keep the branch, `branch_fate: kept_unmerged`.

**Deletion failure** never blocks or reverts completion (it runs after step 3). It is recorded as `branch_fate: delete_failed` with the error; the watcher retries on its tick up to **5** attempts, then stops and leaves the record. `wd plan show` prints it; `wd workspace clean` is the manual fallback.

## 5. Manual `wd plan complete`

**Decision: as recommended.**

- **Gate** (in `Finalize`, so API, CLI and MCP share it; applies only to plans that have an integration branch): refuse when the branch exists, has commits not on the default branch (`git rev-list --count origin/<default>..<branch>` > 0), and there is no merged PR `<branch> → <default>` whose head SHA is the branch tip. A squash-merged PR therefore passes. HTTP 422, message: `integration branch <branch> has <n> commits that are not on <default> (final PR #<n> is open | no final PR). Merge the PR, or pass --abandon-unmerged to complete the plan and keep the branch.`
- **Override:** `--abandon-unmerged`, interactive confirmation naming the branch, `--yes` to skip it. API: `abandon_unmerged: true` on the complete request; MCP `complete_plan` takes the same field.
- **Effect of the override:** the plan completes; the branch is **kept** (local and origin) and recorded as `branch_fate: abandoned`; an open final PR is left open (autopilot never closes it); a live executor is torn down by the normal Finalize cleanup.
- The automatic path never needs the override: the watcher calls `Finalize` only after the merge was observed.

## 6. What the plan record keeps

A new `outcome` object on the plan, written by the daemon and surviving executor teardown:

```yaml
outcome:
  integration_branch: autopilot/<name>
  default_branch: main
  final_pr: {number, url, state: open|merged|closed, head_sha, merged_at}
  branch_fate: deleted | kept_unmerged | abandoned | delete_failed | leftover_unmerged
  branch_deleted_at: <time>
  branch_delete_error: <text>
  branch_delete_attempts: <n>
```

- `integration_branch` / `default_branch` are written when the autopilot run starts. `final_pr` is mirrored from the run status by the completion watcher while the plan is `in_progress` (no new write path out of the controller). `leftover_unmerged` is never stored; it is computed (§9).
- Exposed spec-first on the plan DTO, in `wd plan show` (text and `--json`), and MCP `get_plan` / `list_plans`.

**Why `wd plan show --json` shows no execution summary today.** The summary *is* stored (`Plan.ExecutionSummary`, written by `persistExecutionSummary` during `Finalize`) and the API *does* return it: `GET /api/v1/plans/plan-0edb3aad` contains `execution_summary` and `execution_history`. It is lost on the client: `client.PlanView` (`internal/client/plans_crud.go`), which both the CLI and the MCP tools decode into and re-encode, has no `execution_summary`, `execution_history`, `task_outcomes` or `branch_summaries` fields, so they are dropped. Fix (t3): add the fields to `PlanView` and render them; no storage change. Note the stored summary is thin (for `plan-0edb3aad`: times, `tasks_total`, `tasks_done`, `outcome_note`, empty `goal`); "PRs landed" in the outcome block comes from `execution_history[].plan_branches` / `branch_summaries`, not from new bookkeeping.

## 7. Archiving an in-progress plan

**Decision: as recommended.**

- **Refused (HTTP 409)** while the executor is live: autopilot `starting`, `active`, `paused`, `healing`, `degraded`, `finalizing`, `awaiting_merge`; a pipeline that is still cancelable; an orchestrator/manual plan whose root agent is live. Message: `plan <id> is still running (executor <state>). Stop it first: wd plan stop <id>`.
- **Allowed** when the executor is stopped or absent. Archive then runs the existing plan cleanup (executor record, plan-bound agents, worktrees) and reports what was removed and what was kept.
- **Branches with unmerged commits are kept.** The existing cleanup is not safe for this as written: `CleanupWorktrees` runs `git push origin --delete` on every plan branch unconditionally, and agent teardown removes the worktree with branch deletion. Archive must use a keep-unmerged mode: a branch (worker or integration) is deleted only if it has no commits beyond the default branch or its PR is merged.
- **What `wd plan stop` leaves behind** (checked in `ControlPlan` / `StopRun`): the plan stays **`in_progress`**; only the executor changes (autopilot run → `stopped`, manager terminated; pipeline → canceled). So "stopped in-progress plan" is the normal input to archive.
- Pending and completed plans archive as today.

## 8. Unarchive

**Decision: as recommended.**

- Archive records `archived_from` (`pending | in_progress | completed`) on the plan.
- `wd plan unarchive <plan-id>` (`--json`), `POST /api/v1/plans/{plan_id}/unarchive`, MCP `unarchive_plan`. Returns the plan to `archived_from` and clears `archived_at` / `archived_from`. `canTransition` gains `archived → {pending, in_progress, completed}`, reachable only through unarchive.
- An in-progress plan comes back `in_progress` with a stopped or absent executor; the output says `wd plan restart <id>` continues it. Nothing is started automatically.
- **Plans archived before this change** (no `archived_from`): `completed` if `completed_at` is set, otherwise `pending`. A plan that was archived mid-run before this change therefore comes back `pending` with its task progress intact.
- Refused with 409 on a plan that is not archived.

## 9. Existing data

No automatic status change and no migration step.

- A completed autopilot plan with no `outcome` gets one computed at read time: the integration branch is derived as `autopilot/<plan name>` (the default naming in `internal/autopilot/branch.go`; a repository with a custom `autopilot.merge.target_branch` template is not covered and reports nothing). If that branch exists with commits not on the default branch, `wd plan show` reports `branch_fate: leftover_unmerged` and prints a warning line naming the branch and the commit count.
- `wd plan show` refines this with one gh lookup (a merged PR whose head is the tip means squash-merged, not leftover). `wd plan list --json` flags it from local git only (`integration_branch_leftover: true`) so listing stays offline and cheap; text `plan list` is unchanged.
- `prompt-seed-large-prompt-fix`: at the time of writing `autopilot/prompt-seed-large-prompt-fix` no longer exists locally or on origin, so that plan reports no leftover. The rule above covers the case as it was reported (4 commits ahead, no PR) and any other plan in that shape.

---

## 10. Where each decision lands

| Task | Sections |
|---|---|
| t2-await-final-merge | §1–§5, `outcome` writes of §6 |
| t3-plan-show-ending | §6 (DTO, `PlanView` fix, show output), §9 |
| t4-archive-unarchive | §7, §8 |
| t5-help-text | wording for §1, §4, §5, §8 |
| t6-docs-and-skill | all; amend run-to-final-pr §E.5/§E.6 |

## 11. Verification (contract level)

- Unit: green → `awaiting_merge` with all run agents gone and one notification; merged → complete; head moved → re-gate; conflicting/behind → merge default; closed → `final_pr_closed` park; stale poll result discarded; restart during the wait resumes polling without a manager and without a second PR (including the squash-merged-while-down case).
- Finalize gate and `--abandon-unmerged`; branch deletion success, failure with bounded retry, and kept-because-commits-after-merge.
- The run-to-final-pr end-to-end test is extended: merge the final PR, assert the plan completes and the integration branch is gone locally and on origin.
