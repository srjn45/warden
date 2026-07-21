# Agent Roles

Status: shipped (all four stages — core, surfaces, uis, docs — landed on the
`agent-roles` branch). Issue: *agent roles*.

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

### Built-in roles (exactly five)

| name | persona | defaults |
|------|---------|----------|
| `general` | *(empty)* | none |
| `orchestrator` | coordinates a fleet of warden agents; plans + delegates, does not write feature code itself unless trivial | `permission_mode=auto` |
| `implementer` | implements a task end-to-end on its own branch (code, tests, checks, commit, PR) | `type=development` |
| `auto-merger` | owns getting an open PR merged: monitors CI, fixes failures/conflicts, merges when green | `permission_mode=auto`, `auto_approve=true` |
| `reviewer` | reviews a branch/PR for correctness, coverage, style; findings + verdict, no fixes unless asked | `type=pr-review` |
| `worker` | owns one task end-to-end (implement, self-review, PR, drive green, merge) and reports status back to its coordinator | `type=development`, `permission_mode=auto`, `auto_approve=true` |
| `autopilot` | long-lived headless manager driving a whole autopilot run: decompose, spawn workers/brains, gate + land into the integration branch | `permission_mode=bypassPermissions`, `auto_approve=true` |
| `brain` | on-demand decision resolver: unblocks a stuck agent or makes an ad-hoc design/arch call without human interaction | `permission_mode=auto`, `auto_approve=true` |

The last three (`autopilot`, `worker`, `brain`) form autopilot's manager →
worker → brain topology; see `docs/specs/autopilot.md`.

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
