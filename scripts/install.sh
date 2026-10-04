#!/usr/bin/env bash
# Install warden as a background service.
#
# Two modes:
#   • Dev checkout — `./scripts/install.sh` / `make install` builds from source.
#   • Standalone   — `curl -fsSL …/install.sh | bash` downloads a GitHub release
#                    archive (no git, Go, or npm required).
#
# Idempotent. Flags: --no-build (dev mode only).
set -euo pipefail

# --- bootstrap common.sh ---------------------------------------------------
# When this file lives next to common.sh (git checkout), source it directly.
# When piped via curl|bash, BASH_SOURCE points at a pipe/fd with no sibling —
# fetch common.sh from GitHub into a temp dir and source that instead.
_install_die() { printf 'error: %s\n' "$*" >&2; exit 1; }

_source_common() {
  local dir=""
  if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
    dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    if [ -f "$dir/common.sh" ]; then
      SCRIPT_DIR="$dir"
      # shellcheck source=scripts/common.sh
      source "$dir/common.sh"
      return 0
    fi
  fi

  local ref="${WARDEN_INSTALL_REF:-}"
  if [ -z "$ref" ]; then
    if [ -n "${WARDEN_VERSION:-}" ]; then
      ref="v${WARDEN_VERSION#v}"
    else
      ref="main"
    fi
  fi
  local url="https://raw.githubusercontent.com/srjn45/warden/${ref}/scripts/common.sh"
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/warden-install.XXXXXX")"
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" EXIT
  command -v curl >/dev/null 2>&1 || _install_die "curl is required to bootstrap the installer"
  curl -fsSL "$url" -o "$tmp/common.sh" || _install_die "failed to download common.sh from $url"
  SCRIPT_DIR="$tmp"
  WARDEN_STANDALONE=1
  # shellcheck source=scripts/common.sh
  source "$tmp/common.sh"
}

_source_common

NO_BUILD=0
for arg in "$@"; do
  case "$arg" in
    --no-build) NO_BUILD=1 ;;
    -h|--help)
      cat <<'EOF'
usage: install.sh [--no-build]

Install warden as a user-level background service (launchd on macOS, systemd
--user on Linux).

Dev checkout (./scripts/install.sh / make install):
  Builds from source unless --no-build is passed.

Standalone (curl -fsSL …/install.sh | bash):
  Downloads a pre-built GitHub release archive. No git, Go, or npm required.
  Env:
    WARDEN_VERSION      pin a release (e.g. 9.9.0 or v9.9.0); default: latest
    WARDEN_INSTALL_REF  git ref for fetching common.sh (default: main, or tag)
    WARDEN_ADDR         daemon bind address (default 127.0.0.1:8765)
    WARDEN_INSTALL_MODE force "dev" or "release"
EOF
      exit 0
      ;;
    *) die "unknown argument: $arg" ;;
  esac
done

info "installing warden service"

# Wire git hooks early so even a --no-build run sets them up (cheap, idempotent).
# No-op outside a git checkout.
wire_git_hooks

if is_dev_install && [ "$NO_BUILD" -eq 0 ]; then
  build_release
  deploy_binary
elif is_dev_install && [ "$NO_BUILD" -eq 1 ]; then
  if [ -f "$REPO_ROOT/bin/warden" ]; then
    warn "--no-build: skipping make release, using existing bin/warden"
    deploy_binary
  else
    warn "--no-build and bin/warden missing — falling back to GitHub release download"
    download_release_binary
  fi
else
  info "standalone install: downloading pre-built release from GitHub"
  download_release_binary
fi

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

# Claude skill: symlink the repo copy in dev mode; materialize under
# ~/.warden/skills/warden for standalone installs (no git checkout).
install_claude_skill

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
