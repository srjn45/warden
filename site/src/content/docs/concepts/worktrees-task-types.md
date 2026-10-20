---
title: Worktrees & task types
description: How roles and spawn flags control whether a git worktree is created, and the supervised permission mode.
---

Worktree policy is driven by the **role** and the spawn flags. `--type` is deprecated; use `--role` (required) and `--repo`.

| Spawn | Worktree | Notes |
|---|---|---|
| `--role worker --repo <path> [TICKET]` | yes (new branch) | The default managed mode: isolated `.worktrees/<id>` on a fresh branch (named after the ticket if given). Pass `--branch` to choose the branch name |
| `--role worker --repo <path> --pr <N>` | yes (PR branch) | Runs `gh pr checkout <N>` inside its own worktree. **Exempt from `--in-repo`** — a PR review always gets its own checkout |
| `--role worker --repo <path> --worktree` | yes (scratch) | Throwaway scratch worktree for analysis/spike work |
| `--role worker --repo <path> --in-repo` | no | Opt out of isolation and run in the shared checkout |
| `--role general "<prompt>"` | no | Free-form: runs in the current directory (or `--dir`) |
| `--kind terminal --dir <path>` | no | A plain shell, not an AI agent |

Concurrent managed agents each get their own checkout, so they never trip over each other in the shared tree. The legacy `--type` values (`development`, `code`, `docs`, `website`, `debug-ci`, `tests`, `pr-review`, `analysis`, `spike`, `other`) are still accepted as a deprecated alias and map onto the same behaviour, but new commands should use roles.

By default every agent runs fully autonomously with permission prompts suppressed (on the Claude backend, `claude --dangerously-skip-permissions`; each backend maps to its own bypass flag); the `Notification` hook still records them as events in the session doc.

Pick the permission posture with `--permission-mode` (`acceptEdits|auto|bypassPermissions|default|dontAsk|plan`); the default comes from the `default_permission_mode` config setting. `--supervised` is a kept-for-compatibility alias for `--permission-mode acceptEdits`: file edits and common filesystem commands auto-approve, but other tools (bash writes, network calls, etc.) surface the numbered permission prompt — which the approvals inbox captures and lets you answer from the web AttentionQueue (one-click buttons), the TUI (`⏳ Approvals` row → `i`/`1`-`9`), or the CLI (`warden approval answer`) when approvals are enabled. Change an existing agent's posture with `warden agent set <id> permission-mode <mode>`, or let the daemon answer recognized yes/no prompts for you with `warden approval auto set <id> on` (global default: the `auto_approve` config setting). A restored agent keeps its permission setting.

If a worktree for the ticket already exists on disk, the spawn adopts it (reattaches claude to the existing branch) instead of erroring.

:::note[Worktree policy is not `--task`]
`--role worker --repo` decides **worktree and branch policy**. A separate `--task`
flag (from the built-in *task registry*) decides the **model tier** the
quota-balanced router targets — e.g. `--task architecture` → tier-1,
`--task merge-pr` → tier-3. They are independent flags. See
[Agent roles → Roles, tasks, and tiers](/warden/guides/agent-roles/#roles-tasks-and-tiers).
:::
