# agentctl install / uninstall / reinstall scripts — design

**Date:** 2026-06-02
**Status:** approved, ready for implementation plan

## Problem

The current redeploy loop is `make release && ./bin/agentctl daemon`. This runs the
daemon in the **foreground** — it blocks the terminal and must be killed with ctrl-C.
There is a committed launchd plist (`deploy/com.srajanpathak.agentctl.plist`) and a
documented *manual* install in the README (`cp` + `launchctl load`), but no scripted,
idempotent way to install / uninstall / redeploy the service.

## Goal

Provide three standalone shell scripts that manage agentctl as a detached macOS
launchd service, so the daemon runs in the background, survives terminal close and
login, and can be redeployed with a single command.

## Decisions (locked)

- **Delivery form:** standalone shell scripts under `scripts/`, wrapped by thin
  Makefile targets for convenience.
- **Binary location:** a **stable copy** at `~/.local/bin/agentctl`. The running
  daemon is decoupled from the repo working tree, so rebuilding/moving the repo
  does not disturb the running service.
- **Reinstall scope:** rebuild by default, `--no-build` flag to skip the build and
  just redeploy the existing binary.
- **Install scope:** everything — daemon service + skill symlink + MCP registration.
- **Committed plist:** replaced with a `.template` (single source of truth, rendered
  by the scripts); README manual-install section updated to match.

## Layout

```
scripts/
  common.sh        # sourced library: config, logging, plist render, launchctl helpers
  install.sh       # full first-time setup
  uninstall.sh     # full teardown (data + logs preserved)
  reinstall.sh     # rebuild (default) + redeploy + restart
deploy/
  com.srajanpathak.agentctl.plist.template   # replaces the hardcoded plist
```

Makefile gains thin wrappers:

```make
install:   ; ./scripts/install.sh
uninstall: ; ./scripts/uninstall.sh
reinstall: ; ./scripts/reinstall.sh
```

## `common.sh` — shared library

Sourced by the other scripts; **no side effects on source**. Provides:

**Config constants**
- `LABEL=com.srajanpathak.agentctl`
- `INSTALL_BIN_DIR="$HOME/.local/bin"`, `INSTALL_BIN="$INSTALL_BIN_DIR/agentctl"`
- `PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"`
- `ADDR="127.0.0.1:8765"`
- `REPO_ROOT` — resolved from the script's own location (so scripts work from any cwd).
- `LOG_OUT=/tmp/agentctl.daemon.log`, `LOG_ERR=/tmp/agentctl.daemon.err`

**Logging helpers** — `info()`, `warn()`, `err()`, `die()` with color (TTY-aware;
plain when not a TTY). `die()` prints a red error and exits non-zero.

**Plist rendering** — `render_plist()` reads
`deploy/com.srajanpathak.agentctl.plist.template` and substitutes:
- `__BINARY__` → `$INSTALL_BIN`
- `__HOME__`   → `$HOME` (if needed)
- `__ADDR__`   → `$ADDR`

The template's `PATH` env includes `~/.local/bin` plus homebrew + system paths, so
`claude`, `tmux`, etc. resolve for the daemon. Output is written atomically to `$PLIST`.

**launchctl helpers** (modern with legacy fallback):
- `service_loaded()` — `launchctl print gui/$UID/$LABEL` success ⇒ loaded.
- `load_service()` — `launchctl bootstrap gui/$UID "$PLIST"`; fallback
  `launchctl load -w "$PLIST"`.
- `unload_service()` — `launchctl bootout gui/$UID/$LABEL`; fallback
  `launchctl unload -w "$PLIST"`. No-op (with info) if not loaded.
- `restart_service()` — `launchctl kickstart -k gui/$UID/$LABEL` if loaded, else
  `load_service`.
- `wait_healthy()` — poll `http://$ADDR/healthz` for up to ~5s; return 0 on first
  `200`, non-zero on timeout.

## `install.sh` — full first-time setup

Flags: `--no-build` (skip `make release`).
Steps (each idempotent, each logged):

1. **Build** — `make release` (UI + binary) unless `--no-build`. `die` on failure.
2. **Deploy binary** — `mkdir -p ~/.local/bin`; copy `bin/agentctl` → `~/.local/bin/agentctl`.
3. **Plist** — `render_plist` → `~/Library/LaunchAgents/$LABEL.plist`.
4. **Service** — `restart_service` (loads if absent, kickstarts if present so a
   re-run picks up a new plist/binary).
5. **Skill symlink** — `ln -sfn "$REPO_ROOT/skills/agentctl" ~/.claude/skills/agentctl`
   (same as `make install-skill`).
6. **MCP registration** — if `claude` is on PATH: `claude mcp remove agentctl`
   (ignore errors) then `claude mcp add agentctl --scope user -- agentctl mcp`.
   If `claude` is absent, `warn` and skip (non-fatal).
7. **Verify** — `wait_healthy`; on success print ✅ + URL; on failure print ❌ and
   `tail` the last lines of `/tmp/agentctl.daemon.err`, then exit non-zero.
8. **PATH check** — if `~/.local/bin` not in `$PATH`, `warn` with the line to add to
   the shell profile.

Re-running `install.sh` is safe (overwrites binary/plist, restarts, re-links).

## `reinstall.sh` — the fast redeploy loop

Flags: `--no-build`.
Steps:

1. **Build** — `make release` unless `--no-build`. `die` on failure.
2. **Redeploy binary** — copy `bin/agentctl` → `~/.local/bin/agentctl`.
3. **Restart** — `restart_service` (bootstrap if not yet loaded).
4. **Verify** — `wait_healthy`; report ✅ / ❌-with-logs.

Does **not** re-touch the skill symlink or MCP registration — those are set
idempotently by `install.sh` and don't change on a code redeploy. If the service was
never installed, `restart_service` bootstraps it, so reinstall also works as a
first install of the daemon (minus skill/MCP).

## `uninstall.sh` — full teardown

Flags: `--keep-binary`.
Steps:

1. **Stop service** — `unload_service` (no-op if not loaded).
2. **Remove plist** — `rm -f "$PLIST"`.
3. **Remove binary** — `rm -f ~/.local/bin/agentctl` unless `--keep-binary`.
4. **Remove skill symlink** — `rm -f ~/.claude/skills/agentctl` (only if it's a
   symlink pointing into this repo; otherwise warn and leave it).
5. **Remove MCP registration** — if `claude` present: `claude mcp remove agentctl`
   (ignore errors).

**Preserved (not deleted):** the on-disk session store and `/tmp/agentctl.daemon.*`
logs — data is not the uninstaller's to destroy. Print where they live and the exact
commands to purge them manually.

## Error handling (all scripts)

- `set -euo pipefail` at the top.
- `die()` on any unrecoverable failure (build error, plist write failure, service
  load failure) — red message, non-zero exit.
- External-command failures (launchctl, claude, curl) are caught and surfaced, never
  silently swallowed. `claude`-related steps are non-fatal when `claude` is missing.
- Idempotent / re-runnable: running any script twice produces the same end state.

## Documentation

Update `README.md`:
- Replace the manual `cp` + `launchctl load` install section with
  `./scripts/install.sh` (and the `make install` wrapper).
- Document `reinstall.sh` / `uninstall.sh` and the `--no-build` / `--keep-binary`
  flags.
- Note the new binary location (`~/.local/bin/agentctl`) and the PATH requirement.
- Replace references to `make run-daemon` as the deploy mechanism with the service
  scripts (keep `make run-daemon` documented only as a foreground debug option).

## Out of scope (YAGNI)

- Non-macOS service managers (systemd, etc.) — macOS/launchd only.
- A `agentctl service ...` built-in subcommand — explicitly deferred in favor of
  scripts.
- Purging the session store / logs on uninstall — left to the user.

## Testing / verification

- `install.sh` on a clean machine state → service loads, `/healthz` returns 200,
  `agentctl ls` works, `claude mcp list` shows `agentctl`, skill symlink exists.
- `reinstall.sh` after a code change → daemon serves the new binary (verify via a
  version marker / log line), service stays up.
- `reinstall.sh --no-build` → redeploys existing `bin/agentctl` without rebuilding.
- `uninstall.sh` → service gone (`launchctl print` fails), plist/binary/symlink/MCP
  removed, session store + logs still present.
- Re-running each script is a no-op-equivalent (no errors, same end state).
