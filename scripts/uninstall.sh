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
