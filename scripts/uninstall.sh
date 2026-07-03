#!/usr/bin/env bash
# Tear down the warden service and integrations (launchd on macOS, systemd on Linux).
# Preserves the session store (~/.warden) and daemon logs.
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

info "uninstalling warden service"

unload_service

if [ -f "$SERVICE_CONFIG" ]; then
  rm -f "$SERVICE_CONFIG"
  info "removed $SERVICE_CONFIG"
fi

if [ "$KEEP_BINARY" -eq 0 ]; then
  if [ -e "$INSTALL_BIN" ]; then
    rm -f "$INSTALL_BIN"
    info "removed $INSTALL_BIN"
  fi
  # Remove the `wd` alias only when it still points at our binary.
  if [ -L "$INSTALL_ALIAS" ] && [ "$(readlink "$INSTALL_ALIAS")" = "warden" ]; then
    rm -f "$INSTALL_ALIAS"
    info "removed alias $INSTALL_ALIAS"
  fi
else
  warn "--keep-binary: left $INSTALL_BIN in place"
fi

# Skill symlink — only remove if it points back into this repo.
SKILL_LINK="$HOME/.claude/skills/warden"
if [ -L "$SKILL_LINK" ]; then
  target="$(readlink "$SKILL_LINK")"
  case "$target" in
    "$REPO_ROOT"/*) rm -f "$SKILL_LINK"; info "removed skill symlink" ;;
    *) warn "skill symlink points elsewhere ($target); left in place" ;;
  esac
fi

# MCP registration
if claude_available; then
  if claude mcp remove warden >/dev/null 2>&1; then
    info "removed MCP server registration"
  fi
fi

info "uninstall complete"
if [ "$OS_PLATFORM" = "linux" ]; then
  warn "preserved (not removed): session store ~/.warden"
  info "daemon logs live in the systemd journal (journalctl --user -u warden) and are rotated by systemd"
else
  warn "preserved (not removed): session store ~/.warden and logs $LOG_OUT, $LOG_ERR"
  info "to purge logs:          rm -f $LOG_OUT $LOG_ERR"
fi
info "to purge session store: rm -rf ~/.warden   (deletes all agent records)"
