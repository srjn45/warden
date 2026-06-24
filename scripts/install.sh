#!/usr/bin/env bash
# Install warden as a launchd service: build, deploy binary, plist, load,
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

info "installing warden service"

# Wire git hooks early so even a --no-build run sets them up (cheap, idempotent).
wire_git_hooks

if [ "$NO_BUILD" -eq 0 ]; then
  build_release
else
  warn "--no-build: skipping make release, using existing bin/warden"
fi

deploy_binary
ensure_token          # provisions ~/.warden/token.env when ADDR is non-loopback
render_plist

# Migrate the legacy data dir (~/.agentctl) to ~/.warden before the daemon
# loads, so existing sessions/state carry over on the rename. One-time, only when
# the new dir does not yet exist.
if [ ! -d "$HOME/.warden" ] && [ -d "$HOME/.agentctl" ]; then
  mv "$HOME/.agentctl" "$HOME/.warden"
  info "migrated data dir ~/.agentctl -> ~/.warden"
fi

# Create the config file on a fresh install, or migrate it in place on upgrade
# (adds any missing keys, preserves existing values/comments). Runs before the
# service starts so the daemon loads a fully-populated file.
"$INSTALL_BIN" config init && info "config ready: ~/.warden/config.yaml"

restart_service

# Claude skill symlink (matches 'make install-skill')
mkdir -p "$HOME/.claude/skills"
ln -sfn "$REPO_ROOT/skills/warden" "$HOME/.claude/skills/warden"
info "linked skill -> ~/.claude/skills/warden"

# MCP server registration (idempotent: remove-then-add).
# May be blocked by enterprise MCP policy; degrade to a warning that surfaces why.
if claude_available; then
  claude mcp remove warden >/dev/null 2>&1 || true
  if mcp_out="$(claude mcp add warden --scope user -- warden mcp 2>&1)"; then
    info "registered MCP server 'warden' (user scope)"
  else
    warn "MCP auto-registration skipped: ${mcp_out:-unknown error}"
    warn "register manually if needed — see README 'Orchestrator (MCP)'"
  fi
else
  warn "claude CLI not on PATH; skipped MCP registration"
fi

check_path
report_health
auth_notice
info "install complete"
