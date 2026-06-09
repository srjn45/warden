# agentctl install/uninstall/reinstall scripts — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the foreground `make release && ./bin/agentctl daemon` loop with three idempotent shell scripts that install, redeploy, and remove agentctl as a detached macOS launchd service.

**Architecture:** A sourced shell library (`scripts/common.sh`) holds config, logging, plist rendering, and launchctl helpers. Three thin entry scripts (`install.sh`, `reinstall.sh`, `uninstall.sh`) compose those helpers. The daemon binary is installed to a stable `~/.local/bin/agentctl` (decoupled from the repo tree); the launchd plist is rendered from a committed `.template`. Makefile targets wrap the scripts.

**Tech Stack:** Bash, launchctl (macOS launchd), `sed` for templating, `curl` for the `/healthz` readiness probe, existing `make release` for builds.

---

## Reference facts (verified against the codebase)

- Daemon health endpoint: `GET /healthz` (`internal/daemon/api.go:100`).
- Default listen addr: `127.0.0.1:8765`, env `AGENTCTL_ADDR` (`internal/config/config.go`).
- Session store dir: `~/.agentctl`, env override `AGENTCTL_DATA_DIR` (`internal/config/config.go:30-36`). **Never deleted by uninstall.**
- launchd logs: `/tmp/agentctl.daemon.log`, `/tmp/agentctl.daemon.err` (from plist).
- Existing committed plist: `deploy/com.srajanpathak.agentctl.plist` (label `com.srajanpathak.agentctl`) — to be replaced by a `.template`.
- `claude` CLI is at `~/.local/bin/claude`; `claude mcp add <name> <cmd> [args...]` with `--scope` is available.
- `shellcheck` is **not** installed on this machine — lint steps run it only if present.
- Build: `make release` (builds UI then `bin/agentctl`). `make install-skill` already symlinks the skill.

## File Structure

- Create: `deploy/com.srajanpathak.agentctl.plist.template` — plist with `__BINARY__`/`__ADDR__`/`__HOME__` placeholders.
- Delete: `deploy/com.srajanpathak.agentctl.plist` — superseded by the template.
- Create: `scripts/common.sh` — sourced library (config + logging + plist + launchctl helpers). No side effects beyond defining vars/functions.
- Create: `scripts/install.sh` — full setup: build → binary → plist → service → skill → MCP → verify.
- Create: `scripts/reinstall.sh` — build (default) → binary → plist → restart → verify.
- Create: `scripts/uninstall.sh` — stop service → remove plist/binary/skill/MCP; preserve data + logs.
- Modify: `Makefile` — add `install`, `uninstall`, `reinstall` targets.
- Modify: `README.md` — replace manual install section with the scripts; document flags and binary location.

---

### Task 1: Plist template

**Files:**
- Create: `deploy/com.srajanpathak.agentctl.plist.template`
- Delete: `deploy/com.srajanpathak.agentctl.plist`

- [ ] **Step 1: Create the template**

Create `deploy/com.srajanpathak.agentctl.plist.template` with exactly this content:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.srajanpathak.agentctl</string>
  <key>ProgramArguments</key>
  <array>
    <string>__BINARY__</string>
    <string>daemon</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>AGENTCTL_ADDR</key><string>__ADDR__</string>
    <key>PATH</key><string>__HOME__/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/tmp/agentctl.daemon.log</string>
  <key>StandardErrorPath</key><string>/tmp/agentctl.daemon.err</string>
</dict>
</plist>
```

- [ ] **Step 2: Remove the old hardcoded plist**

Run: `git rm deploy/com.srajanpathak.agentctl.plist`
Expected: `rm 'deploy/com.srajanpathak.agentctl.plist'`

- [ ] **Step 3: Verify placeholders are present and well-formed**

Run: `grep -c '__BINARY__\|__ADDR__\|__HOME__' deploy/com.srajanpathak.agentctl.plist.template`
Expected: `3`

Run (substitution sanity check produces valid XML):
```bash
sed -e 's|__BINARY__|/Users/x/.local/bin/agentctl|g' \
    -e 's|__ADDR__|127.0.0.1:8765|g' \
    -e 's|__HOME__|/Users/x|g' \
    deploy/com.srajanpathak.agentctl.plist.template | plutil -lint -
```
Expected: `- : OK` (plutil reports the piped stdin as valid plist)

- [ ] **Step 4: Commit**

```bash
git add deploy/com.srajanpathak.agentctl.plist.template
git commit -m "feat(deploy): plist template with binary/addr/home placeholders"
```

---

### Task 2: Shared library `scripts/common.sh`

**Files:**
- Create: `scripts/common.sh`
- Test: inline shell checks (no test framework for shell in this repo)

- [ ] **Step 1: Write `scripts/common.sh`**

Create `scripts/common.sh` with exactly this content:

```bash
#!/usr/bin/env bash
# Shared library for agentctl service scripts.
# Source this file ("source common.sh"); do not execute it directly.
# Defines config vars + helper functions only — no service-mutating side effects.

# --- config ---------------------------------------------------------------
LABEL="com.srajanpathak.agentctl"
ADDR="${AGENTCTL_ADDR:-127.0.0.1:8765}"
INSTALL_BIN_DIR="$HOME/.local/bin"
INSTALL_BIN="$INSTALL_BIN_DIR/agentctl"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_OUT="/tmp/agentctl.daemon.log"
LOG_ERR="/tmp/agentctl.daemon.err"
UID_NUM="$(id -u)"

# Resolve repo root from this file's location (scripts/ sits at the repo root).
_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$_COMMON_DIR/.." && pwd)"
TEMPLATE="$REPO_ROOT/deploy/$LABEL.plist.template"

# --- logging --------------------------------------------------------------
if [ -t 1 ]; then
  _C_RED=$'\033[31m'; _C_YEL=$'\033[33m'; _C_GRN=$'\033[32m'; _C_RST=$'\033[0m'
else
  _C_RED=""; _C_YEL=""; _C_GRN=""; _C_RST=""
fi
info() { printf '%s==>%s %s\n' "$_C_GRN" "$_C_RST" "$*"; }
warn() { printf '%swarn:%s %s\n' "$_C_YEL" "$_C_RST" "$*" >&2; }
err()  { printf '%serror:%s %s\n' "$_C_RED" "$_C_RST" "$*" >&2; }
die()  { err "$*"; exit 1; }

claude_available() { command -v claude >/dev/null 2>&1; }

# --- build & binary -------------------------------------------------------
build_release() {
  info "building release (make release)…"
  make -C "$REPO_ROOT" release || die "make release failed"
}

deploy_binary() {
  [ -f "$REPO_ROOT/bin/agentctl" ] || die "bin/agentctl not found — build first (run without --no-build, or 'make release')"
  mkdir -p "$INSTALL_BIN_DIR"
  cp "$REPO_ROOT/bin/agentctl" "$INSTALL_BIN" || die "failed to copy binary to $INSTALL_BIN"
  info "installed binary -> $INSTALL_BIN"
}

# --- plist ----------------------------------------------------------------
render_plist() {
  [ -f "$TEMPLATE" ] || die "plist template not found: $TEMPLATE"
  mkdir -p "$(dirname "$PLIST")"
  local tmp="$PLIST.tmp.$$"
  sed -e "s|__BINARY__|$INSTALL_BIN|g" \
      -e "s|__ADDR__|$ADDR|g" \
      -e "s|__HOME__|$HOME|g" \
      "$TEMPLATE" > "$tmp" || { rm -f "$tmp"; die "failed to render plist"; }
  mv -f "$tmp" "$PLIST"
  info "wrote $PLIST"
}

# --- launchctl ------------------------------------------------------------
service_loaded() {
  launchctl print "gui/$UID_NUM/$LABEL" >/dev/null 2>&1
}

load_service() {
  if launchctl bootstrap "gui/$UID_NUM" "$PLIST" 2>/dev/null; then
    info "service bootstrapped"
  elif launchctl load -w "$PLIST" 2>/dev/null; then
    info "service loaded (legacy)"
  else
    die "failed to load service"
  fi
}

unload_service() {
  if ! service_loaded; then
    info "service not loaded; nothing to stop"
    return 0
  fi
  if launchctl bootout "gui/$UID_NUM/$LABEL" 2>/dev/null; then
    info "service booted out"
  elif launchctl unload -w "$PLIST" 2>/dev/null; then
    info "service unloaded (legacy)"
  else
    warn "could not stop service (it may already be gone)"
  fi
}

restart_service() {
  if service_loaded; then
    launchctl kickstart -k "gui/$UID_NUM/$LABEL" || die "failed to restart service"
    info "service restarted"
  else
    load_service
  fi
}

# --- health ---------------------------------------------------------------
report_health() {
  local url="http://$ADDR/healthz" i
  for i in $(seq 1 25); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then
      info "${_C_GRN}daemon healthy${_C_RST} — http://$ADDR"
      return 0
    fi
    sleep 0.2
  done
  err "daemon did not become healthy at $url"
  if [ -f "$LOG_ERR" ]; then
    warn "last lines of $LOG_ERR:"
    tail -n 15 "$LOG_ERR" >&2 || true
  fi
  return 1
}

# --- PATH advisory --------------------------------------------------------
check_path() {
  case ":$PATH:" in
    *":$INSTALL_BIN_DIR:"*) ;;
    *) warn "$INSTALL_BIN_DIR is not on your PATH. Add to your shell profile:"
       printf '    export PATH="%s:$PATH"\n' "$INSTALL_BIN_DIR" >&2 ;;
  esac
}
```

- [ ] **Step 2: Syntax-check the library**

Run: `bash -n scripts/common.sh`
Expected: no output, exit 0.

- [ ] **Step 3: Lint if shellcheck is available**

Run: `command -v shellcheck >/dev/null && shellcheck scripts/common.sh || echo "shellcheck not installed — skipped"`
Expected: `shellcheck not installed — skipped` (or a clean shellcheck pass).

- [ ] **Step 4: Functionally test `render_plist` in isolation**

This proves the template substitution works without touching the real `~/Library/LaunchAgents`.

Run:
```bash
HOME=/tmp/agentctl-test-home AGENTCTL_ADDR=127.0.0.1:9999 bash -c '
  set -euo pipefail
  source scripts/common.sh
  render_plist
  echo "--- rendered ---"
  cat "$PLIST"
'
```
Expected: output contains `==> wrote /tmp/agentctl-test-home/Library/LaunchAgents/com.srajanpathak.agentctl.plist`, and the rendered plist shows `/tmp/agentctl-test-home/.local/bin/agentctl` as the program, `127.0.0.1:9999` as the addr, and **no** remaining `__` placeholders.

- [ ] **Step 5: Verify the rendered test plist is valid and has no placeholders**

Run:
```bash
plutil -lint /tmp/agentctl-test-home/Library/LaunchAgents/com.srajanpathak.agentctl.plist
grep -c '__' /tmp/agentctl-test-home/Library/LaunchAgents/com.srajanpathak.agentctl.plist || true
```
Expected: `... OK` from plutil, and grep count `0`.

- [ ] **Step 6: Clean up the test artifacts**

Run: `rm -rf /tmp/agentctl-test-home`
Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add scripts/common.sh
git commit -m "feat(scripts): shared library for agentctl service management"
```

---

### Task 3: `scripts/install.sh`

**Files:**
- Create: `scripts/install.sh`

- [ ] **Step 1: Write `scripts/install.sh`**

Create `scripts/install.sh` with exactly this content:

```bash
#!/usr/bin/env bash
# Install agentctl as a launchd service: build, deploy binary, plist, load,
# link the Claude skill, and register the MCP server. Idempotent.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/common.sh
source "$SCRIPT_DIR/common.sh"

NO_BUILD=0
for arg in "$@"; do
  case "$arg" in
    --no-build) NO_BUILD=1 ;;
    -h|--help) echo "usage: install.sh [--no-build]"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

info "installing agentctl service"

if [ "$NO_BUILD" -eq 0 ]; then
  build_release
else
  warn "--no-build: skipping make release, using existing bin/agentctl"
fi

deploy_binary
render_plist
restart_service

# Claude skill symlink (matches 'make install-skill')
mkdir -p "$HOME/.claude/skills"
ln -sfn "$REPO_ROOT/skills/agentctl" "$HOME/.claude/skills/agentctl"
info "linked skill -> ~/.claude/skills/agentctl"

# MCP server registration (idempotent: remove-then-add)
if claude_available; then
  claude mcp remove agentctl >/dev/null 2>&1 || true
  if claude mcp add agentctl --scope user -- agentctl mcp >/dev/null 2>&1; then
    info "registered MCP server 'agentctl' (user scope)"
  else
    warn "could not register MCP server automatically; see README 'Orchestrator (MCP)'"
  fi
else
  warn "claude CLI not on PATH; skipped MCP registration"
fi

check_path
report_health
info "install complete"
```

- [ ] **Step 2: Syntax-check**

Run: `bash -n scripts/install.sh`
Expected: no output, exit 0.

- [ ] **Step 3: Make executable**

Run: `chmod +x scripts/install.sh`
Expected: no output.

- [ ] **Step 4: Argument-parsing test (no side effects)**

Run: `scripts/install.sh --bogus 2>&1; echo "exit=$?"`
Expected: `error: unknown argument: --bogus` and `exit=1`.

Run: `scripts/install.sh --help; echo "exit=$?"`
Expected: `usage: install.sh [--no-build]` and `exit=0`.

- [ ] **Step 5: Real install run**

Run: `scripts/install.sh`
Expected: builds, prints `installed binary -> ~/.local/bin/agentctl`, `wrote ~/Library/LaunchAgents/...plist`, a bootstrap/restart line, skill link line, MCP registration line, and ends with `daemon healthy — http://127.0.0.1:8765` then `install complete`.

- [ ] **Step 6: Verify the service and integration**

Run:
```bash
launchctl print "gui/$(id -u)/com.srajanpathak.agentctl" | grep -E 'state|program ='
curl -fsS http://127.0.0.1:8765/healthz && echo " <- healthz ok"
test -L ~/.claude/skills/agentctl && echo "skill linked"
claude mcp list 2>/dev/null | grep agentctl || echo "(mcp list unavailable)"
```
Expected: launchctl shows `state = running` and `program = .../.local/bin/agentctl`; healthz ok; `skill linked`; agentctl present in mcp list.

- [ ] **Step 7: Idempotency check — run install again**

Run: `scripts/install.sh --no-build`
Expected: completes without error, ends `daemon healthy` + `install complete`, service still running (re-runnable).

- [ ] **Step 8: Commit**

```bash
git add scripts/install.sh
git commit -m "feat(scripts): install.sh — set up agentctl launchd service + skill + MCP"
```

---

### Task 4: `scripts/reinstall.sh`

**Files:**
- Create: `scripts/reinstall.sh`

- [ ] **Step 1: Write `scripts/reinstall.sh`**

Create `scripts/reinstall.sh` with exactly this content:

```bash
#!/usr/bin/env bash
# Redeploy agentctl: rebuild (default), recopy the binary, restart the service.
# This replaces the old `make release && ./bin/agentctl daemon` loop.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/common.sh
source "$SCRIPT_DIR/common.sh"

NO_BUILD=0
for arg in "$@"; do
  case "$arg" in
    --no-build) NO_BUILD=1 ;;
    -h|--help) echo "usage: reinstall.sh [--no-build]"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

info "reinstalling agentctl daemon"

if [ "$NO_BUILD" -eq 0 ]; then
  build_release
else
  warn "--no-build: redeploying existing bin/agentctl"
fi

deploy_binary
render_plist          # keep plist in sync; harmless if unchanged
restart_service       # bootstraps if not yet loaded
report_health
info "reinstall complete"
```

- [ ] **Step 2: Syntax-check**

Run: `bash -n scripts/reinstall.sh`
Expected: no output, exit 0.

- [ ] **Step 3: Make executable**

Run: `chmod +x scripts/reinstall.sh`
Expected: no output.

- [ ] **Step 4: Real reinstall run (full rebuild)**

Run: `scripts/reinstall.sh`
Expected: runs `make release`, `installed binary -> ~/.local/bin/agentctl`, `service restarted`, `daemon healthy`, `reinstall complete`.

- [ ] **Step 5: Verify the running binary is the freshly built one**

Run:
```bash
cmp -s bin/agentctl ~/.local/bin/agentctl && echo "installed binary matches freshly built bin/agentctl"
curl -fsS http://127.0.0.1:8765/healthz && echo " <- still healthy after restart"
```
Expected: `installed binary matches freshly built bin/agentctl` and healthz ok.

- [ ] **Step 6: Fast path — `--no-build`**

Run: `scripts/reinstall.sh --no-build`
Expected: prints `--no-build: redeploying existing bin/agentctl` (no `make release`), restarts, `daemon healthy`, `reinstall complete`.

- [ ] **Step 7: Commit**

```bash
git add scripts/reinstall.sh
git commit -m "feat(scripts): reinstall.sh — rebuild + redeploy + restart daemon"
```

---

### Task 5: `scripts/uninstall.sh`

**Files:**
- Create: `scripts/uninstall.sh`

- [ ] **Step 1: Write `scripts/uninstall.sh`**

Create `scripts/uninstall.sh` with exactly this content:

```bash
#!/usr/bin/env bash
# Tear down the agentctl launchd service and integrations.
# Preserves the session store (~/.agentctl) and daemon logs.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/common.sh
source "$SCRIPT_DIR/common.sh"

KEEP_BINARY=0
for arg in "$@"; do
  case "$arg" in
    --keep-binary) KEEP_BINARY=1 ;;
    -h|--help) echo "usage: uninstall.sh [--keep-binary]"; exit 0 ;;
    *) die "unknown argument: $arg" ;;
  esac
done

info "uninstalling agentctl service"

unload_service

if [ -f "$PLIST" ]; then
  rm -f "$PLIST"
  info "removed $PLIST"
fi

if [ "$KEEP_BINARY" -eq 0 ]; then
  if [ -e "$INSTALL_BIN" ]; then
    rm -f "$INSTALL_BIN"
    info "removed $INSTALL_BIN"
  fi
else
  warn "--keep-binary: left $INSTALL_BIN in place"
fi

# Skill symlink — only remove if it points back into this repo.
SKILL_LINK="$HOME/.claude/skills/agentctl"
if [ -L "$SKILL_LINK" ]; then
  target="$(readlink "$SKILL_LINK")"
  case "$target" in
    "$REPO_ROOT"/*) rm -f "$SKILL_LINK"; info "removed skill symlink" ;;
    *) warn "skill symlink points elsewhere ($target); left in place" ;;
  esac
fi

# MCP registration
if claude_available; then
  if claude mcp remove agentctl >/dev/null 2>&1; then
    info "removed MCP server registration"
  fi
fi

info "uninstall complete"
warn "preserved (not removed): session store ~/.agentctl and logs $LOG_OUT, $LOG_ERR"
info "to purge logs:          rm -f $LOG_OUT $LOG_ERR"
info "to purge session store: rm -rf ~/.agentctl   (deletes all agent records)"
```

- [ ] **Step 2: Syntax-check**

Run: `bash -n scripts/uninstall.sh`
Expected: no output, exit 0.

- [ ] **Step 3: Make executable**

Run: `chmod +x scripts/uninstall.sh`
Expected: no output.

- [ ] **Step 4: Real uninstall run**

Run: `scripts/uninstall.sh`
Expected: `service booted out`, `removed ~/Library/LaunchAgents/...plist`, `removed ~/.local/bin/agentctl`, `removed skill symlink`, `removed MCP server registration`, `uninstall complete`, and the preserved-data notices.

- [ ] **Step 5: Verify full teardown + data preservation**

Run:
```bash
launchctl print "gui/$(id -u)/com.srajanpathak.agentctl" >/dev/null 2>&1 && echo "STILL LOADED (bad)" || echo "service gone (good)"
test -f ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist && echo "plist present (bad)" || echo "plist gone (good)"
test -e ~/.local/bin/agentctl && echo "binary present (bad)" || echo "binary gone (good)"
test -e ~/.agentctl && echo "session store preserved (good)" || echo "session store missing (note: only bad if it existed before)"
```
Expected: `service gone (good)`, `plist gone (good)`, `binary gone (good)`, and the session store preserved if it existed.

- [ ] **Step 6: Idempotency — run uninstall again**

Run: `scripts/uninstall.sh`
Expected: completes without error; prints `service not loaded; nothing to stop` and skips already-absent items, still ends `uninstall complete`.

- [ ] **Step 7: Re-install to leave the machine in a working state**

Run: `scripts/install.sh`
Expected: `install complete` + `daemon healthy` (restores the service after the teardown test).

- [ ] **Step 8: Commit**

```bash
git add scripts/uninstall.sh
git commit -m "feat(scripts): uninstall.sh — tear down service, preserve data"
```

---

### Task 6: Makefile wrappers

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add the targets to `.PHONY` and define them**

In `Makefile`, change the `.PHONY` line from:

```make
.PHONY: build test lint run-daemon ui ui-dev web-test release install-skill
```

to:

```make
.PHONY: build test lint run-daemon ui ui-dev web-test release install-skill install uninstall reinstall
```

Then add these targets at the end of the file:

```make
# Install agentctl as a launchd service (build + binary + plist + skill + MCP).
install:
	./scripts/install.sh

# Tear down the launchd service and integrations (preserves data + logs).
uninstall:
	./scripts/uninstall.sh

# Rebuild and redeploy the running daemon (use NO_BUILD=1 to skip the build).
reinstall:
	./scripts/reinstall.sh $(if $(NO_BUILD),--no-build,)
```

- [ ] **Step 2: Verify the targets are wired up**

Run: `make -n reinstall NO_BUILD=1`
Expected: prints `./scripts/reinstall.sh --no-build` (dry-run, no execution).

Run: `make -n install`
Expected: prints `./scripts/install.sh`.

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "feat(make): install/uninstall/reinstall targets wrapping scripts"
```

---

### Task 7: README documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Replace the manual install section**

Find the section beginning `## Install the daemon as a launchd service (auto-start)` (around line 33) and its manual `cp`/`launchctl load` instructions (through the `launchctl unload` snippet near line 58). Replace the body of that section with:

````markdown
## Install the daemon as a launchd service (auto-start)

Install with the script — it builds the release, installs the binary to
`~/.local/bin/agentctl`, renders and loads the launchd plist, links the Claude
skill, and registers the MCP server:

```sh
./scripts/install.sh        # or: make install
```

The daemon then starts automatically at login and restarts on crash
(`KeepAlive = true`), listening on `127.0.0.1:8765` by default.

> `~/.local/bin` must be on your `PATH` to run `agentctl` from the shell — the
> installer warns if it isn't.

**Redeploy after a code change** (replaces `make release && ./bin/agentctl daemon`):

```sh
./scripts/reinstall.sh             # rebuild UI + binary, redeploy, restart
./scripts/reinstall.sh --no-build  # redeploy the existing build only
# or: make reinstall  /  make reinstall NO_BUILD=1
```

**Uninstall** (stops and removes the service, binary, skill link, and MCP
registration; **preserves** your session store at `~/.agentctl` and the logs):

```sh
./scripts/uninstall.sh                 # or: make uninstall
./scripts/uninstall.sh --keep-binary   # leave ~/.local/bin/agentctl in place
```

Logs:

- stdout: `/tmp/agentctl.daemon.log`
- stderr: `/tmp/agentctl.daemon.err`
````

- [ ] **Step 2: Soften the `make run-daemon` references**

Search the README for `make run-daemon` used as the *deploy* mechanism (e.g. near lines 367-368 and 392). Leave the command documented, but reframe it as a **foreground debugging** option, not the normal way to run the daemon. For example, change a line like:

```
make run-daemon   # or: launchctl load ~/Library/LaunchAgents/com.srajanpathak.agentctl.plist
```

to:

```
./scripts/install.sh   # install + start as a background launchd service (recommended)
make run-daemon        # foreground, for debugging only (blocks the terminal; ctrl-C to stop)
```

- [ ] **Step 3: Verify no stale manual-install instructions remain**

Run: `grep -n "cp deploy/com.srajanpathak.agentctl.plist\|launchctl load ~/Library\|launchctl unload ~/Library" README.md || echo "no stale manual install refs"`
Expected: `no stale manual install refs`.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document install/reinstall/uninstall scripts; demote run-daemon to debug"
```

---

## Final verification

- [ ] **End-to-end:** from a clean state, `make install` → service running, `/healthz` 200, `agentctl ls` works, skill linked, MCP registered.
- [ ] **Redeploy loop:** make a trivial code change, `make reinstall` → new binary serving (verify `cmp -s bin/agentctl ~/.local/bin/agentctl`), service stayed managed.
- [ ] **Teardown:** `make uninstall` → service/plist/binary/skill/MCP gone, `~/.agentctl` intact.
- [ ] **All scripts re-runnable:** running install/reinstall/uninstall twice yields the same end state with no errors.
- [ ] `bash -n` clean on all four scripts; `git status` clean after commits.

## Notes for the implementer

- These scripts mutate real machine state (launchd, `~/.local/bin`, `~/.claude`, MCP config). The verification steps run the actual scripts — that is intentional and expected for this task.
- If `claude mcp add agentctl --scope user -- agentctl mcp` fails on this `claude` version, check `claude mcp add --help` for the exact flag (`--scope` value or transport) and adjust the one line in `install.sh`; the script already degrades to a warning rather than failing the install.
- macOS/launchd only by design (see spec "Out of scope").
