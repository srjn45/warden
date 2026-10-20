---
title: Roles, tiers, and model routing
description: Choose an agent persona, understand how its model tier resolves, and see how warden selects an eligible backend and model.
---

Every AI-agent spawn has two independent choices:

- A **role** says *who the agent is*: its persistent persona and safe default posture.
- A **model tier** says *how much capability the work needs*. It lets warden select a backend and model without hard-coding either into every spawn.

Roles are a fixed built-in catalog. Model-tier mappings, model catalog entries, and backend availability live in the backend registry. This separation matters: changing a role does not select a particular provider, and choosing a provider does not change the agent's persona.

## Built-in roles

Use the installed catalog rather than copying role names into automation:

```sh
warden agent role list
```

| Role | Purpose | Spawn defaults |
|---|---|---|
| `general` | Plain agent with no injected persona. | None. |
| `autopilot` | Long-lived manager for one autonomous run. It decomposes work, coordinates workers, and keeps the run moving. | Bypass-permissions posture, auto-approve, `autopilot` tag. |
| `brain` | Short-lived, decisive resolver for a blocked agent or an ad-hoc design decision. | Bypass-permissions posture and auto-approve. |
| `orchestrator` | Coordinates a fleet: plans, delegates, monitors, and resolves blockers. | Bypass-permissions posture and auto-approve. |
| `planner` | Research and planning only; produces specs, RFCs, designs, and tradeoff analysis. | Plan/read-only posture. |
| `worker` | Delivers one implementation task: changes, checks, self-review, PR, and coordinator report. | Automatic/allowlisted posture and auto-approve. |

The role name is stored on the session; its persona is resolved again whenever the agent launches. Changing a running role relaunches that agent, so its in-flight turn is discarded:

```sh
warden agent set agent-abc123 role worker
warden agent get agent-abc123 role --json
```

`implementer`, `reviewer`, and `auto-merger` remain accepted compatibility names; each resolves to `worker`. They are not separate roles.

## Execution profiles

A role's defaults form its execution profile. `planner` is deliberately
read-only/plan-oriented; `worker` is allowlisted for normal implementation;
`autopilot`, `brain`, and `orchestrator` receive the bypass-permissions posture
needed to coordinate unattended work. Warden maps those role contracts to the
selected AI CLI's own permission vocabulary at launch.

For ordinary defaults, an explicit spawn value takes precedence over a role
default, which takes precedence over the global default. The built-in
non-`general` roles are stricter for permission mode: their canonical role mode
is enforced even when a caller supplies another mode. Role tags are added to the
supplied tags rather than replacing them; a role that enables auto-approval
keeps it enabled. This is why a role is more than a display label, while still
remaining independent of model selection.

## Tasks describe the work

`--task` is a routing input, not an identity. The built-in task registry maps work to a model tier and restricts tasks to compatible roles:

| Model tier | Typical tasks |
|---|---|
| `tier-1` | `analysis`, `architecture`, `design`, `research`, `spike` |
| `tier-2` | `code-review`, `development`, `docs`, `pr-review` |
| `tier-3` | `debug-ci`, `merge-pr`, `monitor-ci`, `release` |

For example, a planner doing an architecture study and a worker writing docs can both be routed without spelling out a model:

```sh
warden agent start --role planner --task architecture "design the sync protocol"
warden agent start --role worker --task docs --repo . DOCS-42
```

`--task` is different from the deprecated `--type`: `--type` controls legacy worktree/branch policy, while `--task` selects a model tier. See [Worktrees and task types](/warden/concepts/worktrees-task-types/) for that distinction.

## How a target tier resolves

When no exact backend and model are pinned, warden determines the target tier in this order:

```text
explicit --tier
  > tier assigned to --task
  > default tier mapped to --role
  > tier-2
```

Inspect or change the durable role-to-tier mapping with:

```sh
warden agent role tier list
warden agent role tier set worker tier-2
```

The initial mappings are `tier-1` for `autopilot`, `orchestrator`, and `planner`; `tier-2` for `general`, `brain`, and `worker`. They are seed values, not hard-coded role properties: the registry preserves an operator's later mapping change.

You can pin a tier when the task name is not a good fit:

```sh
warden agent start --role worker --tier tier-1 "investigate a difficult regression"
```

## From tier to backend and model

The spawn command applies this precedence:

```text
--aicli + --model      exact pair
--aicli                choose a model for that CLI in the resolved tier
--tier or --task       choose an eligible model in that tier
--role                 choose an eligible model in the role's mapped tier
configured defaults    fallback when routing cannot supply a target
```

`--model` requires `--aicli`. An exact pair is an operator pin; otherwise the resolver considers only models marked for automatic assignment in the selected tier. It excludes missing or disabled backend CLIs, disabled or retired models, models unavailable to the active account, and candidates in a rate-limit cooldown. Unknown usage is eligible for a trial rather than being invented.

Among eligible candidates, warden normally picks the greatest quota headroom and round-robins ties. A pool resetting within one hour with at least 10% headroom is used first, so subscription capacity that would soon expire is not wasted. Where fallback is enabled by the caller, an exhausted `tier-1` can fall through to `tier-2` then `tier-3`; an exhausted `tier-2` can fall through to `tier-3`.

Inspect the catalog that supplies those candidates with:

```sh
warden models list --by-tier
warden models list --tier tier-2
```

## The backend registry's part

Do not confuse **model tiers** above with a backend's **billing tier** (`free`, `subscription`, `pay_per_use`, or `unclassified`). The backend registry records installed/enabled state, billing preference, quota observations, and the models assigned to each capability tier. It is therefore the resolver's source of eligibility and headroom, while the role mapping supplies its starting tier.

Standard agent routing accepts installed, enabled subscription backends by default; pay-per-use candidates require the caller to allow them. The registry also supplies Autopilot's separate cost ladder. Manage backend facts and billing preferences in the [Backend registry guide](/warden/guides/backend-registry/), and use the [model-catalog rollout guide](/warden/guides/model-catalog-rollout/) when refreshing account-scoped model availability.

## API and UI surfaces

The CLI supports `--role`, `--task`, and `--tier` on `warden agent start`. The REST spawn body and MCP `spawn_agent` also accept `role`, `task`, and `tier`; pipeline jobs accept `role` and `tier` (not `task`). The TUI exposes role and tier pickers, while the web new-agent form exposes the role picker.
