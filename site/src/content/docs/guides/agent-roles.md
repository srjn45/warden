---
title: Agent roles
description: Attach a built-in persona (orchestrator, planner, worker, autopilot, brain) plus default spawn flags and a default model tier to an agent — at spawn or on a running agent.
---

A **role** is a named, persistent system-prompt **persona** attached to an agent —
*who the agent is* — plus a set of default spawn flags and a default model tier.
Every agent has exactly one role. The default is `general`, which injects no
persona and behaves exactly as agents did before roles existed — so an untouched
spawn is byte-identical to today.

The role set is a **fixed built-in catalog**. There are no user-defined roles.
(What the agent is *doing* is a separate axis — the **task** — which drives model
tiering; see [Roles, tasks, and tiers](#roles-tasks-and-tiers) below.)

## The built-in roles

```sh
warden agent role list
```

| Role | What the persona tells the agent to do | Default flags | Default tier |
|---|---|---|---|
| `general` | *(no persona — a plain agent)* | — | tier-2 |
| `orchestrator` | Coordinate a fleet of warden agents to deliver a goal: break the work into tasks, spawn and assign worker agents via the warden MCP/CLI, monitor progress, resolve blockers, integrate results. Plan and delegate; don't write feature code yourself unless it's trivial. | `--permission-mode auto` | tier-1 |
| `planner` | Research, analysis, and planning **only**: produce tech specs, RFCs, design docs, and tradeoff analyses. Read and search the codebase to inform the work, but don't edit source, config, or tests. | `--permission-mode plan` | tier-1 |
| `worker` | Own one task end-to-end: implement (code + tests + checks + commit), self-review the diff, open a PR on your integration branch, drive it to green, and merge — then report status back to your coordinator. | `--type development`, `--permission-mode auto`, auto-approve on | tier-2 |
| `autopilot` | Long-lived headless **manager** that drives a whole autopilot run: decompose the goal, spawn `worker`/`brain` agents, gate their PRs, and land into the integration branch — fully unattended. | `--permission-mode bypassPermissions`, auto-approve on | tier-1 |
| `brain` | On-demand **decision resolver**: unblock a stuck agent or make an ad-hoc design/architecture call, decisively and without human interaction, then report the resolution back. | `--permission-mode auto`, auto-approve on | tier-2 |

`autopilot`, `worker`, and `brain` power [autopilot](/warden/guides/autopilot/)'s
topology: the `autopilot` manager spawns `worker` agents to deliver tasks and
`brain` agents to resolve blockers.

:::note[Legacy role aliases]
`reviewer`, `implementer`, and `auto-merger` are **no longer first-class roles** —
the work they named is now expressed as a **task** (`pr-review`, `development`,
`merge-pr`; see below). All three names still resolve to the `worker` role, so
`--role reviewer` and existing presets keep working.
:::

## Choose a role at spawn

Pass `--role` on `warden start`:

```sh
# The role's persona is injected and its default flags fill anything you leave unset
warden start "review PR 1234 for correctness" --role worker

# An explicit flag always overrides the role's default
warden start PROJ-9 --role worker --type spike   # spike wins over the role's development default
```

**Precedence** for each default: an explicit request value beats the role default
beats the global default. Default `tags` are **unioned** into whatever tags you
passed (deduplicated), never replacing them, and `auto_approve` is OR-ed in. A
role's default **tier** feeds the model router the same way — overridden by a task
or an explicit `--tier` (see below).

The same choice is available in the UIs and over MCP:

- **TUI** — the new-agent form has a `ctrl+r` **role picker** (↑/↓ or `j`/`k` to
  cycle, `enter` to choose); the footer shows the selected `role:` and it defaults
  to `general`.
- **Web** — the **+ New agent** modal has a **Role** dropdown (defaults `general`),
  with the selected role's description shown beside it.
- **MCP** — `spawn_agent` takes a `role` param; `list_roles` returns the catalog
  for a picker.

## Roles, tasks, and tiers

A role says *who the agent is*. A separate axis — the **task** — says *what it is
doing*, and both feed warden's **quota-balanced model router**, which picks each
spawn's backend + model by headroom within a **model tier**.

- **`--task <name>`** — a unit of work from the built-in **task registry** (the
  canonical task→tier source). Each task carries a tier:

  | Tier | Tasks |
  |---|---|
  | tier-1 (deep reasoning) | `analysis`, `architecture`, `design`, `research`, `spike` |
  | tier-2 (standard dev) | `code-review`, `development`, `docs`, `pr-review` |
  | tier-3 (mechanical) | `debug-ci`, `merge-pr`, `monitor-ci`, `release` |

- **`--tier <tier>`** — pin the tier directly (`tier-1` / `tier-2` / `tier-3`).

The **target tier** resolves with the precedence **explicit `--tier` > task tier >
role default tier > tier-2**. Within the tier, the router scores every model by
quota headroom (`1 − used/limit`), skips rate-limited or ineligible backends, and
picks the highest-headroom candidate (round-robin among ties). A pinned
`--backend`/`--model` **bypasses** the router, and a first spawn **degrades** to
the request defaults if routing is unavailable — it never hard-fails.

```sh
warden start "design the sync protocol" --role planner     # role → tier-1
warden start PROJ-9 --type development --task development   # task → tier-2
warden start "cut the v9 release" --task release           # task → tier-3
warden start "urgent hotfix" --tier tier-1                 # pin the tier directly
```

:::caution[`--task` is not `--type`]
`--type` decides worktree/branch policy
([worktrees & task types](/warden/concepts/worktrees-task-types/)); `--task`
decides the model tier. The two name-sets overlap but are independent flags.
:::

**Surfaces:** `--task` and `--tier` are on `warden start` and the REST spawn body;
`tier:` (alongside `role:`) is also a pipeline-job field — the pipeline Job spec
has no `task:`. Over MCP, `spawn_agent` routes by `role` only (its default tier
feeds the router — no `task`/`tier` params).

## Switch a running agent's role

```sh
warden agent role set agent-abc123 worker    # give the running agent the worker persona
warden agent role set agent-abc123 general   # clear the persona (back to a plain agent)
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
