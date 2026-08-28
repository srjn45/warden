# Agent Roles

Status: shipped (all four stages — core, surfaces, uis, docs — landed on the
`agent-roles` branch). Issue: *agent roles*.

> **Update (tier trio):** the role catalog has since been split from the **task**
> dimension. The *task-like* roles that used to live here (`implementer`,
> `auto-merger`, `reviewer`) were removed as first-class roles and are now
> expressed as **tasks** (see [Roles vs. tasks](#roles-vs-tasks-two-dimensions)
> below); the names remain accepted as **back-compat aliases** that resolve to the
> `worker` role. A `planner` role was added. Every role also carries a **default
> model tier** that feeds the quota-balanced resolver at spawn — see
> [Tier-at-spawn resolution](#tier-at-spawn-resolution) and
> [`docs/specs/tiered-model-routing.plan.md`](tiered-model-routing.plan.md).

A **role** is a named, persistent system-prompt PERSONA attached to an agent,
plus an optional set of default spawn flags. Every agent has exactly one role;
the empty role is the built-in `general` role, which injects no persona and
behaves exactly as agents do today. The role set is a **fixed built-in set** —
there are no user-defined roles for now.

This document is the source of truth for the whole feature. It is delivered as a
four-stage pipeline:

1. **core** (this stage) — data model, the `internal/role` registry, spawn-time
   resolution + persona injection. No CLI/MCP/API/TUI/web/docs surfaces.
2. **surfaces** — CLI (`--role`, `wd role …`), MCP tool params, daemon API
   fields.
3. **uis** — TUI + web role picker / display.
4. **docs** — README, `docs/`, website, skill.

---

## 1. Data model

`store.Session` gains one field, mirroring the existing `PermissionMode`:

```go
Role string `json:"role,omitempty"` // built-in role name; empty ⇒ "general"
```

Empty means the built-in `general` role. The field stores the role **name**
only — never the persona text. The persona is re-resolved from the registry at
every (re)launch, so switching a role and resuming re-injects automatically with
nothing persona-shaped persisted on disk beyond the name.

`store.FileStore` gains a narrow setter mirroring `UpdatePermissionMode`:

```go
func (fs *FileStore) UpdateRole(ctx context.Context, id, role string) error
```

added to the `store.Store` interface alongside `UpdatePermissionMode`. It funnels
through the shared `mutate` read-modify-write primitive.

## 2. The `internal/role` registry

Built-in roles are embedded YAML, mirroring `internal/pipeline/templates`
(`//go:embed roles/*.yaml`). Each role is one YAML file:

```yaml
name: implementer
description: Implements a task end-to-end on its own branch.
persona: |
  You are an implementer. ...
defaults:
  type: development
  model: ""
  permission_mode: ""
  auto_approve: false
  tags: []
```

Go types:

```go
type Defaults struct {
    Type           string   `yaml:"type"`
    Model          string   `yaml:"model"`
    PermissionMode string   `yaml:"permission_mode"`
    AutoApprove    bool     `yaml:"auto_approve"`
    Tags           []string `yaml:"tags"`
}

type Role struct {
    Name        string   `yaml:"name"`
    Description string   `yaml:"description"`
    Persona     string   `yaml:"persona"`
    Defaults    Defaults `yaml:"defaults"`
}
```

API:

- `role.Get(name string) (Role, bool)` — look up by name. `general` (and the
  empty string, normalized to `general`) always resolve. Unknown names return
  `false`; it is the **call site's** job to turn that into an error.
- `role.Names() []string` — all role names, sorted, `general` first.
- `role.All() []Role` — all roles, same ordering.
- `role.Default = "general"` — the default role name.

The registry is loaded once at package init from the embedded YAML; a malformed
embedded file panics at init (it is a build-time asset, never user input).

### Built-in roles

The catalog is six roles (source of truth: `internal/role/roles/*.yaml`). Each row
also lists its **default model tier** (`internal/backendstore` `DefaultRoleTiers`),
which the router consults when nothing more specific pins the tier.

| name | persona | defaults | default tier |
|------|---------|----------|--------------|
| `general` | *(empty)* | none | tier-2 |
| `orchestrator` | coordinates a fleet of warden agents; plans + delegates, does not write feature code itself unless trivial | `permission_mode=auto` | tier-1 |
| `planner` | research/analysis/planning only — produces specs, RFCs, design docs; must not edit code | `permission_mode=plan` | tier-1 |
| `worker` | owns one task end-to-end (implement, self-review, PR, drive green, merge) and reports status back to its coordinator | `type=development`, `permission_mode=auto`, `auto_approve=true` | tier-2 |
| `autopilot` | long-lived headless manager driving a whole autopilot run: decompose, spawn workers/brains, gate + land into the integration branch | `permission_mode=bypassPermissions`, `auto_approve=true`, `tags=[autopilot]` | tier-1 |
| `brain` | on-demand decision resolver: unblocks a stuck agent or makes an ad-hoc design/arch call without human interaction | `permission_mode=auto`, `auto_approve=true` | tier-2 |

`autopilot`, `worker`, and `brain` form autopilot's manager → worker → brain
topology; see `docs/specs/autopilot.md`.

**Legacy aliases.** `reviewer`, `implementer`, and `auto-merger` are no longer
first-class roles — the work they named is now a **task** (`pr-review`,
`development`, `merge-pr`). `role.Get` still maps all three to the `worker` role,
so existing spawns, presets, and `--role reviewer` keep working; the CLI/MCP
`--role` help still lists them for discoverability.

## Roles vs. tasks (two dimensions)

A spawn now has two orthogonal, optional axes plus a backend:

- **Role** (`--role`) — *who the agent is*: a persistent persona + default spawn
  flags + a default model tier. Fixed catalog above; persists on the session and
  re-injects at every (re)launch.
- **Task** (`--task`) — *what the agent is doing*: a named unit of work from the
  **task registry** (`internal/task`, embedded `tasks/*.yaml`). Each task carries a
  **tier** and the set of roles it is valid for. It is a routing input only — it is
  not persisted as an identity the way a role is.

The task registry (13 built-in tasks) is the **canonical task→tier source**
(`task.TierFor`); role→tier mappings never encode task tiers:

| tier | tasks |
|------|-------|
| tier-1 | `analysis`, `architecture`, `design`, `research`, `spike` |
| tier-2 | `code-review`, `development`, `docs`, `pr-review` |
| tier-3 | `debug-ci`, `merge-pr`, `monitor-ci`, `release` |

> **`--task` is not `--type`.** The `--type` worktree types (`development`,
> `docs`, `pr-review`, `analysis`, `spike`, …) decide branch/worktree policy; the
> `--task` registry decides the **model tier**. The two name-sets overlap but are
> independent flags.

## Tier-at-spawn resolution

`router.Resolver.DetermineTargetTier` picks the target tier with this precedence
(top wins):

```
explicit --tier
  > task tier          (task.TierFor, when --task is set)
  > role default tier  (backendstore.GetRoleTier, when --role is set)
  > tier-2 default
```

The resolved tier drives candidate selection: the resolver scores every model in
that tier by **quota headroom** (`1 − used/limit`), filters out ineligible or
rate-limited (≥ threshold, default 90%) backends, and picks the highest-headroom
candidate (round-robin among ties). See the routing plan for the full algorithm.

**Backend + model at first spawn** (`lifecycle.resolveSpawnTarget`) layers on top,
mirroring how a hot-swap picks its successor but never hard-failing:

1. a pinned `--backend` wins outright (keeps `--model`, or the backend default);
2. a pinned or **role-default** `--model` (folded into the request before this runs)
   on the default backend wins over the router;
3. otherwise the quota-balanced router picks backend+model from
   `{Role, Task, Tier, AllowFallback}`;
4. with **no resolver wired**, or on any resolver error / no-candidate, the spawn
   **degrades** to the request's backend+model — a first spawn never fails because
   routing is absent or empty.

**Surfaces.** `--tier` / `--task` are on `warden start` (CLI) and the daemon REST
spawn body (`tier` / `task`). `tier:` (alongside `role:`) is also a **pipeline job**
field (`pipeline.Job`) — the Job spec has **no** `task:`. Over **MCP**,
`spawn_agent` routes by `role` only (its default tier feeds the resolver) — it does
not take `tier`/`task` params.

## 3. Spawn resolution + persona injection

`lifecycle.SpawnRequest` gains `Role string` (and, so the `auto_approve` default
has a spawn field to fill, `AutoApprove bool`).

### Resolution (precedence)

At the top of `Spawn`, before free-form vs typed is decided (the `type` default
can flip a spawn from free-form to typed), the role is resolved:

1. Normalize the role name (empty ⇒ `general`); an unknown name is an error.
2. For each of `type`, `model`, `permission_mode`, `auto_approve`: the role
   default fills the request field **only when the caller left it unset**.
   Precedence is **explicit request value > role default > global default**.
   `auto_approve` is a bool with no tri-state, so the role default is OR-ed in
   (`req.AutoApprove || roleDefault`) — an explicit `true` and a role default of
   `true` both enable it; the global default is `false`.
3. `tags` are **UNIONED**: the role's default tags are added to the request's
   tags (normalized, de-duplicated), never replacing them.
4. The resolved role **name** is persisted on the `Session` (`sess.Role`).

### Injection

The persona is injected as a system-prompt addendum through the **same seam**
warden already uses for its collab/git/pipeline hints — no new mechanism:

- **Claude** (flag-based, `Caps.SystemPromptInject`): the persona is one more
  guidance string threaded through `systemPromptHints`, which file-backs the
  text under `HintsDir` and references it via a single
  `--append-system-prompt "$(cat <file>)"`. This keeps the tmux launch line
  small — the persona is **never** inlined onto the launch line (the 1024-byte
  MAX_CANON limit would truncate it and stop the agent starting). Every launch
  value stays `shellQuoteArg`-ed.
- **Injecting backends** (Codex/OpenCode/Cursor/Antigravity/Crush/Goose,
  `ContextInjector`): the persona is one more guidance string threaded through
  `injectContext`, which writes it into the backend's rules-file drop
  (`AGENTS.md` etc.) inside the warden-delimited block. It is **prepended**
  ahead of the collab/git hints so the persona reads first.
- **Aider** (neither seam): degrades silently, exactly like the other hints.

The persona is **always-on** when non-empty (not gated by a config setting,
unlike collab/pipeline/git hints). An empty persona (`general`, or any role with
no persona) injects nothing, leaving the launch byte-identical to today.

Because the persona is re-resolved from the registry at every (re)launch and only
the role **name** is stored, switching a role + resuming re-injects the new
persona automatically — there is no persona text on disk to migrate.

## 4. Later stages (out of scope for core)

- **surfaces**: `spawn --role`, a `wd role list|set` (switch = `UpdateRole` +
  relaunch/resume re-injection), MCP `spawn_agent`/`set_role` params, daemon
  `SpawnRequest.role` + a role-set endpoint.
- **uis**: TUI role column + picker; web role badge + selector.
- **docs**: README/FEATURES/USAGE, website guide + reference, skill.
