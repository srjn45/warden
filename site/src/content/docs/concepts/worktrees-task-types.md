---
title: Worktrees & task types
description: How the --type flag controls whether a git worktree is created, and the supervised permission mode.
---

The `--type` flag controls whether a git worktree is created and determines how the session is set up. **Write types** — types whose job is to change the codebase — get their own isolated worktree by default so concurrent agents never trip over each other in the shared checkout.

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | Creates `.worktrees/<ticket>` on a new branch named after the ticket |
| `code` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo root |
| `docs` | yes (new branch) | Isolated worktree by default; `--in-repo` to opt out |
| `website` | yes (new branch) | Isolated worktree by default; `--in-repo` to opt out |
| `debug-ci` | yes (new branch) | Isolated worktree by default; `--in-repo` to opt out |
| `tests` | yes (new branch) | Isolated worktree by default; `--in-repo` to opt out |
| `pr-review` | yes (PR branch) | Detached worktree; runs `gh pr checkout <PR>` inside it. Requires `--pr` or `--branch`. **Exempt from `--in-repo`** — a PR review always gets its own checkout |
| `analysis` | opt-in (`--worktree`) | Read-only by intent; runs in the repo by default, pass `--worktree` for a scratch branch |
| `spike` | opt-in (`--worktree`) | Same as analysis |
| `other` | no | Catch-all; also used for unrecognized type strings |

The five write types (`code`, `docs`, `website`, `debug-ci`, `tests`) join `development` in getting an isolated `.worktrees/<id>` checkout on a fresh branch unless you pass `--in-repo`. `pr-review` always gets its own checkout and ignores `--in-repo`. The read-only types (`analysis`, `spike`, `other`) run in the repo root unless you ask for a scratch worktree with `--worktree`.

By default every agent runs fully autonomously with permission prompts suppressed (on the Claude backend, `claude --dangerously-skip-permissions`; each backend maps to its own bypass flag); the `Notification` hook still records them as events in the session doc.

Pick the permission posture with `--permission-mode` (`acceptEdits|auto|bypassPermissions|default|dontAsk|plan`); the default comes from the `default_permission_mode` config setting. `--supervised` is a kept-for-compatibility alias for `--permission-mode acceptEdits`: file edits and common filesystem commands auto-approve, but other tools (bash writes, network calls, etc.) surface the numbered permission prompt — which the approvals inbox captures and lets you answer from the web AttentionQueue (one-click buttons), the TUI (`⏳ Approvals` row → `i`/`1`-`9`), or the CLI (`warden approve`) when approvals are enabled. Change an existing agent's posture with `warden set-permission-mode <id> <mode>`, or let the daemon answer recognized yes/no prompts for you with `warden auto-approve <id> on` (global default: the `auto_approve` config setting). A restored agent keeps its permission setting.

If a worktree for the ticket already exists on disk, the spawn adopts it (reattaches claude to the existing branch) instead of erroring.
