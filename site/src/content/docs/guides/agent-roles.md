---
title: Agent roles
description: Attach a built-in persona (orchestrator, implementer, auto-merger, reviewer) plus default spawn flags to an agent — at spawn or on a running agent.
---

A **role** is a named, persistent system-prompt **persona** attached to an agent,
plus a set of default spawn flags. Every agent has exactly one role. The default
is `general`, which injects no persona and behaves exactly as agents did before
roles existed — so an untouched spawn is byte-identical to today.

The role set is a **fixed built-in catalog**. There are no user-defined roles.

## The built-in roles

```sh
warden role list
```

| Role | What the persona tells the agent to do | Default flags |
|---|---|---|
| `general` | *(no persona — a plain agent)* | — |
| `orchestrator` | Coordinate a fleet of warden agents to deliver a goal: break the work into tasks, spawn and assign implementer/reviewer agents via the warden MCP/CLI, monitor progress, resolve blockers, integrate results. Plan and delegate; don't write feature code yourself unless it's trivial. | `--permission-mode auto` |
| `implementer` | Implement a task end-to-end on your own branch: write the code and tests, run the project checks, commit, and open a PR. Keep scope tight. | `--type development` |
| `auto-merger` | Own getting an open PR merged: monitor its CI, fix failures or conflicts, push and re-check, and merge as soon as it's green and approved. | `--permission-mode auto`, auto-approve on |
| `reviewer` | Review the changes on a branch/PR for correctness, test coverage, and style. Produce clear, actionable findings and a merge/blocker verdict. Don't implement fixes unless asked. | `--type pr-review` |

## Choose a role at spawn

Pass `--role` on `warden start`:

```sh
# The role's persona is injected and its default flags fill anything you leave unset
warden start "review PR 1234 for correctness" --role reviewer

# An explicit flag always overrides the role's default
warden start PROJ-9 --role implementer --type spike   # spike wins over the role's development default
```

**Precedence** for each default: an explicit request value beats the role default
beats the global default. Default `tags` are **unioned** into whatever tags you
passed (deduplicated), never replacing them, and `auto_approve` is OR-ed in.

The same choice is available in the UIs and over MCP:

- **TUI** — the new-agent form has a `ctrl+r` **role picker** (↑/↓ or `j`/`k` to
  cycle, `enter` to choose); the footer shows the selected `role:` and it defaults
  to `general`.
- **Web** — the **+ New agent** modal has a **Role** dropdown (defaults `general`),
  with the selected role's description shown beside it.
- **MCP** — `spawn_agent` takes a `role` param; `list_roles` returns the catalog
  for a picker.

## Switch a running agent's role

```sh
warden set-role agent-abc123 reviewer   # give the running agent the reviewer persona
warden set-role agent-abc123 general    # clear the persona (back to a plain agent)
```

`set-role` persists the new role **name** and **relaunches** the agent so the new
persona re-injects. A persona only takes effect at (re)launch, so — unlike
`set-permission-mode`, which just updates a stored value — this discards the
agent's in-flight turn. The MCP equivalent is `set_role {ticket, role}`.

## How it works under the hood

- **Only the role name is stored** on the session (`Session.Role`; empty ⇒
  `general`, so pre-roles stores need no migration). The persona text is **never**
  written to disk.
- **The persona re-resolves at every (re)launch** from the embedded `internal/role`
  registry. Because nothing persona-shaped is persisted, switching a role and
  resuming re-injects the new persona automatically.
- **Injection reuses warden's existing system-prompt seam** — the same one used for
  its collab/git/pipeline hints:
  - **Claude** receives it via `--append-system-prompt`, file-backed under
    `HintsDir` and never inlined onto the tmux launch line (so the 1024-byte
    MAX_CANON limit can't truncate it and stop the agent starting).
  - **Injecting backends** (Codex, OpenCode, Cursor, Antigravity, Crush, Goose) get
    it **prepended** into their rules-file drop (`AGENTS.md`, `CRUSH.md`,
    `.goosehints`) so the persona reads first.
  - **Aider**, which has no injection seam, degrades silently like the other hints.
- **An empty persona injects nothing.** `general`, or any role without a persona,
  leaves the launch byte-identical to a plain agent.
