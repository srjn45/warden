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
