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

# MCP server registration (idempotent: remove-then-add).
# May be blocked by enterprise MCP policy; degrade to a warning that surfaces why.
if claude_available; then
  claude mcp remove agentctl >/dev/null 2>&1 || true
  if mcp_out="$(claude mcp add agentctl --scope user -- agentctl mcp 2>&1)"; then
    info "registered MCP server 'agentctl' (user scope)"
  else
    warn "MCP auto-registration skipped: ${mcp_out:-unknown error}"
    warn "register manually if needed — see README 'Orchestrator (MCP)'"
  fi
else
  warn "claude CLI not on PATH; skipped MCP registration"
fi

check_path
report_health
info "install complete"
