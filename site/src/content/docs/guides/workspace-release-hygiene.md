---
title: Workspace & release hygiene
description: Reclaim merged worktrees safely, keep agent git lifecycle actions on warden rails, and get SemVer release advice without creating a tag.
---

The end of a change has three separate jobs: land the work through the normal pull-request flow, remove the worktrees and branches that are now safely merged, and decide whether the accumulated changes merit a release. Warden has dedicated commands for each job so that cleanup and release work stay deliberate and auditable.

## Keep the git lifecycle on warden rails

From an agent worktree, use the lifecycle commands rather than hand-rolling a sequence of raw Git commands:

```sh
wd commit -m "docs: explain workspace hygiene"
wd push
wd sync
wd check
wd git pr --title "docs: explain workspace hygiene"
```

These commands stage and commit work, push the agent branch, rebase it onto its configured base, run the repository's configured checks, and open (or return) the pull request. The rails protect default branches, use force-with-lease for a required force push, refuse a dirty-tree sync, run pre-commit hooks, and associate the action with the agent record. `wd sync` resolves its base from the session, then an autopilot integration branch, then the repository default branch.

If a sync conflicts, resolve the named files and use `wd sync --continue`; use `wd sync --abort` to abandon that rebase. Run `wd check` before opening or updating the pull request. See [Lifecycle commands & rails](/warden/guides/lifecycle-and-rails/) for lifecycle options and the hook-based redirects that steer agents away from raw commit, push, rebase, and broad project-check commands.

## Clean work that has already landed

`warden workspace clean` finds branches whose work has landed on the base branch and the stale worktrees checked out on them. It also recognizes squash merges, using merged pull requests when available and Git-based checks otherwise. `wd clean` is its top-level alias.

Start with a preview:

```sh
wd workspace clean --dry-run
# equivalent shortcut
wd clean --dry-run
```

The report separates stale worktrees, merged local branches, and merged remote branches. Without `--dry-run`, warden shows the same preview and asks for confirmation before it removes worktrees, then deletes local branches, then deletes remote branches.

```sh
wd clean                 # preview, then answer the confirmation prompt
wd clean --local-only    # do not fetch or inspect/delete origin branches
wd clean --json          # machine-readable report or result
```

`clean` fetches and prunes `origin` before its normal detection. It never deletes `main`, `master`, `develop`, `release/*`, the base branch, or the primary worktree's branch. Dirty worktrees are retained unless `--force` is supplied; `--force` still does not bypass branch protection.

Do not confuse cleanup of merged work with orphan reclamation. `wd workspace prune` reclaims **orphaned warden worktrees** whose agent record is gone. `wd workspace clean` targets **merged branches** and their stale worktrees. Use `--dry-run` when assessing either command, especially before approving a cleanup in a shared repository.

## Ask for release advice after the final PR lands

Once the final pull request for a plan is merged, `warden release` inspects commits since the latest SemVer tag. It recommends a major, minor, or patch bump; calculates the next `vMAJOR.MINOR.PATCH`; and renders a categorized changelog from conventional commits and merged pull requests. `wd release` is the short form.

```sh
wd release --dry-run
```

The default analysis target is `origin/main` (or `origin/master`), not an active feature worktree. Use `--target <ref>` to review another target and `--no-fetch` only when intentionally avoiding the usual fetch. A squash commit can still receive a release bump when its body contains releasable conventional-commit bullets.

By default, the command asks before creating an annotated tag and, with `--push`, asks again before pushing it. Existing tags are never overwritten, and a `none` recommendation creates nothing. A pushed `v*` tag triggers the release pipeline, so agents should use `wd release --dry-run` only. Pass `--yes` only when the operator has explicitly authorized creating a tag; do not treat a recommendation as approval to release.

## A safe closing sequence

1. Commit, push, run `wd check`, and open the pull request through the lifecycle commands.
2. Wait for the pull request to land through the repository's normal integration process.
3. Preview `wd workspace clean --dry-run`, then confirm cleanup only for merged work that the report identifies.
4. After the plan's final PR has landed, run `wd release --dry-run` to review the proposed SemVer bump and changelog.
5. Have the release owner decide whether to create and push a tag.

This keeps unmerged work intact, makes cleanup reviewable, and separates release advice from the authority to publish a release.
