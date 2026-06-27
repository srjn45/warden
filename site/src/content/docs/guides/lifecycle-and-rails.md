---
title: Lifecycle commands & boundary enforcement
description: First-class git/check verbs (commit, push, sync, check) on warden rails, plus the PreToolUse hooks that keep agents inside their worktree.
---

warden moves the deterministic, error-prone parts of an agent's job — git and running checks — off Claude and onto **first-class commands**, then *enforces* the worktree boundary with PreToolUse hooks. The agent stops spending tokens narrating `git status`/`add`/`commit` round-trips, and parallel agents stop colliding in a shared checkout.

## The lifecycle verbs

All four are CLI commands **and** MCP tools, and all take `--json`. They run on the agent's pinned worktree branch.

```sh
warden commit -m "fix: handle nil token"   # stage + commit the whole worktree
warden commit                              # …or let warden write the message
warden push                                # push the branch to origin (sets upstream)
warden sync --base main                    # fetch + rebase onto origin/main
warden check                               # run every configured check
warden check test                          # run one (test / lint / build / …)
```

### Rails

The verbs refuse the mistakes a raw git session makes:

- **No commits or pushes on `main`/`master`** — push your agent branch and open a PR.
- **`sync` refuses a dirty tree** (commit first) and, on conflict, leaves the rebase in progress reporting *only* the conflicting files.
- **Pre-commit hooks run**, and a hook failure comes back as a structured result instead of a wall of output.
- **Each action is linked to the agent** for the audit trail.

### Auto-written commit messages

Omit `-m` and warden fills the message: if a local model is configured (`local_llm`), it distils a Conventional-Commits subject from the staged diff; otherwise a deterministic conventional message is derived from the changed paths. A blank commit is impossible.

### `warden check` and `.warden/check.yml`

`check` runs the commands declared in the project's `.warden/check.yml` and returns a pass/fail summary with captured output **for the failing checks only** — in place of the hundreds of lines a raw test run spills into the transcript (the single biggest token win). Commands come from the project, so warden stays language-agnostic; a repo with no `.warden/check.yml` has nothing to run. Per-entry `dir:` supports monorepos.

## Boundary enforcement (PreToolUse hooks)

The verbs are only half the story — warden also keeps agents *on* them. Each agent is launched with a per-agent `claude --settings` file carrying PreToolUse hooks. Every hook **fails open** (a hook error never blocks the agent) and is individually config-gated (default on). The layering goes *steer first, then deny-and-redirect*.

| Hook | Config key | What it does |
|---|---|---|
| **Prompt steer** | `rails.git_conventions` | A system-prompt hint nudging agents toward `wd commit`/`push`/`sync`/`check` over raw git/test Bash — the gentle first layer. |
| **Isolation guard** | `rails.isolation_guard` | Denies an isolated agent's Edit/Write that escapes its worktree into the shared repo. |
| **Git-guard** | `rails.git_redirect` | Argv-parses each Bash command and deny-redirects raw `git commit`/`push`/`pull`/`rebase` to the warden verbs (reads stay allowed); the deny message names the exact replacement. |
| **Check-guard** | `rails.check_redirect` | Deny-redirects a raw test/lint/build command that `.warden/check.yml` registers to `wd check` (broad runs redirect; focused `-run` runs pass through). |

Default-isolated write agents (`code`/`docs`/`website`/`debug-ci`/`tests`, see [Worktrees & task types](/warden/concepts/worktrees-task-types/)) are what make the isolation guard meaningful: each gets its own worktree unless you pass `--in-repo`, so the guard has a boundary to enforce and parallel agents never clobber one another.
