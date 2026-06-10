---
title: Worktrees & task types
description: How the --type flag controls whether a git worktree is created, and the supervised permission mode.
---

When you need a managed git worktree (e.g. a development branch tied to a Jira ticket), pass `--type`. The type controls whether a git worktree is created and determines how the session is set up.

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | Creates `.worktrees/<ticket>` on a new branch named after the ticket |
| `pr-review` | yes (PR branch) | Detached worktree; runs `gh pr checkout <PR>` inside it. Requires `--pr` or `--branch` |
| `analysis` | opt-in (`--worktree`) | Runs in the repo by default; pass `--worktree` to get a scratch branch |
| `spike` | opt-in (`--worktree`) | Same as analysis |
| `code` | no | Runs directly in the repo root |
| `docs` | no | Runs directly in the repo root |
| `website` | no | Runs directly in the repo root |
| `debug-ci` | no | Runs directly in the repo root |
| `tests` | no | Runs directly in the repo root |
| `other` | no | Catch-all; also used for unrecognized type strings |

By default every agent runs `claude --dangerously-skip-permissions` — permission prompts are suppressed and the agent runs fully autonomously; the `Notification` hook still records them as events in the session doc.

Pass `--supervised` to opt into a lighter permission mode (`--permission-mode acceptEdits`): file edits and common filesystem commands auto-approve, but other tools (bash writes, network calls, etc.) surface the numbered permission prompt — which the approvals inbox captures and lets you answer from the web AttentionQueue (one-click buttons), the TUI (`⏳ Approvals` row → `i`/`1`-`9`), or the CLI (`warden approve`) when `WARDEN_APPROVALS` is on. A restored agent keeps its supervised setting.

If a worktree for the ticket already exists on disk, the spawn adopts it (reattaches claude to the existing branch) instead of erroring.
