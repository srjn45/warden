# Config File — Design

**Date:** 2026-06-17
**Status:** Approved (pending spec review)

## Problem

All warden feature flags and toggles are currently configured through environment
variables (`WARDEN_*`, with legacy `AGENTCTL_*` fallbacks), read at startup by
`config.Load()` and a handful of scattered `os.Getenv` calls in other packages.
This has two drawbacks:

1. There is no single, persistent, human-editable place to set configuration.
2. There is no way for a user to see the resolved state of all config values.

We will replace env-var configuration with a single YAML config file that the user
edits by hand, document every setting inline with its allowed values, create the
file with defaults at install time, migrate it in place on upgrade (adding only
missing keys while honoring existing values), and add a `warden config` command to
display the live resolved state.

## Decisions

- **File-only configuration.** Environment variables are dropped entirely for
  user-facing settings. The one exception is `WARDEN_SESSION_ID`, which is
  per-agent IPC (the daemon injects it into each spawned agent's tmux env so the
  agent's MCP server knows its own id) — it is not user configuration and stays an
  env var. System env (`TMUX`, `SHELL`) is unaffected. All `AGENTCTL_*` legacy
  fallbacks are removed; the `~/.agentctl` → `~/.warden` data-dir migration in
  `install.sh` stays.
- **Format & location.** YAML at a fixed path `~/.warden/config.yaml`. The location
  is bootstrap state, resolved before (and independent of) the `data_dir` setting
  that the file itself contains. Overridable with a global `--config` flag (a CLI
  argument, not an env var), which also keeps tests clean.
- **Scope.** All user-facing toggles move into the file — the 16 settings already in
  `config.Config` plus 8 currently read via scattered `os.Getenv` calls.
- **Inline hints.** Every key carries a head-comment documenting its allowed values.
- **Architecture: typed struct + parallel schema table.** Keep the existing typed
  `config.Config` (consumers are unchanged) with YAML tags, plus one ordered schema
  table (key → default → hint) used to generate and migrate the file. A
  reflection-based drift-guard test asserts the struct's YAML tags and the schema
  table never diverge. (Rejected alternatives: a fully declarative registry — too
  invasive, rewrites every consumer and loses compile-time field safety; Viper/koanf
  — heavy dependency, fights the drop-env goal, and its merge does not preserve
  comments / unknown keys.)

## Architecture

### Package `internal/config`

Two distinct entry points, splitting the read path from the write path:

- **`Reconcile(path string) error` — the only writer.**
  - If the file is **absent**: generate it in full from the schema table (every key
    with its default value and hint comment) and write it.
  - If the file is **present**: parse it as a `yaml.Node` tree. For each schema key
    **not already present** in the top-level mapping, append a key node (with its
    hint as a head-comment) and a value node (its default). Marshal the node tree
    back and write. Existing values, existing comments, and unknown keys are all
    preserved untouched — `Reconcile` only adds what is missing.
  - The `yaml.Node` API is required because it round-trips comments; a plain struct
    marshal would strip them.

- **`Load(path string) Config` — read-only.**
  - Parse the file, apply schema defaults for any missing keys, then validate.
  - Validation matches today's rules: `default_permission_mode` must be one of
    `auto | default | acceptEdits | bypassPermissions | dontAsk | plan` (invalid →
    log warning, fall back to `auto`); `token_warn < token_critical` (degenerate →
    both reset to defaults `200000` / `400000`). Duration strings are parsed with
    `time.ParseDuration` (invalid → default).
  - Never writes. If the file is absent, returns an all-defaults `Config`.

- **`DefaultPath() string`** returns `~/.warden/config.yaml` (falls back gracefully
  when the home dir is unavailable, mirroring existing `defaultDataDir`).

`Reconcile` runs in two places:
1. **Daemon startup** (`daemon.go` `RunE`, before `Load`) — the primary guarantee
   that the file exists and is migrated, covering goreleaser tarball installs that
   never run `install.sh`.
2. **`install.sh`** — eagerly via `warden config init` after the binary is deployed.

### Schema table

A single ordered slice of descriptors, each `{ Key, Default, Hint }`, is the source
of truth for file generation and reconcile. The typed `Config` struct carries the
matching YAML tags for consumers. A reflection-based test (see Testing) guarantees
the two never drift.

### Settings

Existing 16 (already in `Config`, now with YAML keys): `addr`, `data_dir`,
`claude_projects_dir`, `notify`, `approvals`, `auto_approve`,
`default_permission_mode`, `spawn_gate`, `spawn_gate_max_agents`, `metrics`,
`allow_nonloopback`, `token_guard`, `token_warn_alert`, `token_auto_compact`,
`token_warn`, `token_critical`.

New 8 (migrated from scattered `os.Getenv`):

| YAML key | Type | Default | Replaces |
|---|---|---|---|
| `pipeline_keep_done` | bool | `false` | `WARDEN_PIPELINE_KEEP_DONE` (daemon.go) |
| `model_default` | string | `claude-sonnet-4-5` | `WARDEN_MODEL_DEFAULT` (lifecycle/models.go) |
| `pipeline_hint` | bool | `true` | `WARDEN_NO_PIPELINE_HINT` (lifecycle.go, **inverted**) |
| `auto_restart_max` | int | `3` | `WARDEN_AUTO_RESTART_MAX` (autorestart.go) |
| `auto_restart_reset` | duration | `5m` | `WARDEN_AUTO_RESTART_RESET` (autorestart.go) |
| `rate_limit_retry_interval` | duration | `30m` | `WARDEN_RATE_LIMIT_RETRY_INTERVAL` (ratelimit.go) |
| `rate_limit_buffer` | duration | `1m` | `WARDEN_RATE_LIMIT_BUFFER` (ratelimit.go) |
| `rate_limit_auto_resume` | bool | `true` | `WARDEN_RATE_LIMIT_AUTO_RESUME` (ratelimit.go) |

Durations are stored in YAML as Go duration strings (e.g. `5m`, `30m`).

### Consumer rewiring

The scattered `os.Getenv` reads are replaced by threading `Config` fields into the
relevant constructors:

- `internal/cli/daemon.go`: `WARDEN_PIPELINE_KEEP_DONE` → `cfg.PipelineKeepDone`.
- `internal/lifecycle/models.go`: `resolveDefaultModel` uses `cfg.ModelDefault`.
- `internal/lifecycle/lifecycle.go`: `pipelineHint` uses `cfg.PipelineHint`.
- `internal/daemon/autorestart.go`: `NewRestarter` takes max/reset from `cfg`.
- `internal/daemon/ratelimit.go`: `NewRateLimitScheduler` takes interval/buffer/
  auto-resume from `cfg`.

The exact constructor signatures and threading are left to the implementation plan;
the principle is that `Config` is loaded once in the daemon and passed down rather
than re-read from the environment deep in these packages.

### Example file

```yaml
# warden configuration — edit values below; run `warden config` to see what's live.

# Daemon listen address. Values: host:port (loopback only unless allow_nonloopback)
addr: 127.0.0.1:8765

# Automatically answer recognized yes/no permission prompts. Values: true | false
auto_approve: false

# Default permission mode for new agents.
# Values: auto | default | acceptEdits | bypassPermissions | dontAsk | plan
default_permission_mode: auto

# Default model for new agents. Values: any claude model id or alias (e.g. sonnet, opus)
model_default: claude-sonnet-4-5

# Auto-resume agents after a rate limit. Values: true | false
rate_limit_auto_resume: true
# How long to wait before retrying after a rate limit. Values: Go duration (e.g. 30m, 1h)
rate_limit_retry_interval: 30m
# ... (all A + B settings, each with a hint comment)
```

## Command: `warden config`

- **`warden config`** (default action): prints the resolved live `Config` values,
  grouped, with the file path shown at the top and any validation warnings. This is
  the "see current state" surface.
- **`warden config path`**: prints the resolved config file path.
- **`warden config init`**: force create/migrate the file (idempotent); invoked by
  `install.sh`.

A global `--config <path>` persistent flag (alongside the existing `--addr`)
overrides the file location and is threaded into `Load`/`Reconcile`.

## Install integration

`scripts/install.sh` runs `warden config init` (using the freshly deployed binary)
after `deploy_binary` and before `restart_service`, so a fresh install lands a
fully populated, commented file and an upgrade migrates an existing one. The
defensive `Reconcile` at daemon startup covers install paths that bypass the script.

## Testing

- **Config unit tests** (rewritten): load-with-defaults, absent-file → all defaults,
  validation (permission-mode whitelist, token warn/critical ordering, bad
  durations), and reconcile behavior:
  - create when absent (full file with hints),
  - add only missing keys on an existing file,
  - preserve existing values, unknown keys, and comments.
- **Drift-guard test**: reflect over `Config`, collect YAML tags, assert the set
  equals the schema table's key set (both directions).
- **Test migration**: the ~86 `Setenv` calls across 7 files
  (`config_test.go`, `mcp/server_test.go`, `lifecycle/lifecycle_test.go`,
  `cli/rotate_test.go`, `tui/cockpit_digest_approvals_test.go`,
  `cli/lifecycle_test.go`, `lifecycle/models_test.go`) migrate to writing a temp
  config file (via `--config`) or constructing `Config` directly.

## Out of scope

- `warden config get`/`set` mutation commands (the file is edited by hand).
- `warden config edit` ($EDITOR launcher) — possible later convenience, not now.
- Live config reload / file watching.
- Per-agent config overrides beyond what already exists (e.g. per-agent
  auto-approve via the existing `auto-approve` command).
