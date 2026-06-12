#!/usr/bin/env bash
# Shared library for warden service scripts.
# Source this file ("source common.sh"); do not execute it directly.
# Defines config vars + helper functions only — no service-mutating side effects.

# --- config ---------------------------------------------------------------
LABEL="com.srajanpathak.warden"
# Common-name of the self-signed code-signing cert created by codesign-setup.sh.
# Signing the binary with a stable identity keeps macOS Full Disk Access grants
# valid across rebuilds (see codesign-setup.sh for the full rationale).
CODESIGN_IDENTITY="warden-codesign"
# WARDEN_ADDR is canonical; AGENTCTL_ADDR is still honored as a fallback.
ADDR="${WARDEN_ADDR:-${AGENTCTL_ADDR:-127.0.0.1:8765}}"
INSTALL_BIN_DIR="$HOME/.local/bin"
INSTALL_BIN="$INSTALL_BIN_DIR/warden"
# Short alias symlink (wd -> warden) created alongside the binary.
INSTALL_ALIAS="$INSTALL_BIN_DIR/wd"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_OUT="/tmp/warden.daemon.log"
LOG_ERR="/tmp/warden.daemon.err"
UID_NUM="$(id -u)"

# Resolve repo root from this file's location (scripts/ sits at the repo root).
_COMMON_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$_COMMON_DIR/.." && pwd)"
TEMPLATE="$REPO_ROOT/deploy/$LABEL.plist.template"

# Detect platform; SERVICE_CONFIG is the canonical service-config path for all
# scripts — plist on macOS, systemd unit on Linux.
OS_PLATFORM="$(uname -s | tr '[:upper:]' '[:lower:]')"
SERVICE_CONFIG="$PLIST"   # default: macOS plist; overridden for Linux below

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
  [ -f "$REPO_ROOT/bin/warden" ] || die "bin/warden not found — build first (run without --no-build, or 'make release')"
  mkdir -p "$INSTALL_BIN_DIR"
  cp "$REPO_ROOT/bin/warden" "$INSTALL_BIN" || die "failed to copy binary to $INSTALL_BIN"
  info "installed binary -> $INSTALL_BIN"
  # Short alias: `wd` -> warden (the alias is provided purely by argv0).
  ln -sfn warden "$INSTALL_ALIAS"
  info "linked alias -> $INSTALL_ALIAS"
  codesign_binary
  # The binary was (re)written and (re)signed, so its code-signature cdhash
  # changed. launchd pins a Lightweight Code Requirement (LWCR) to the running
  # job's signature; `kickstart -k` reuses that stale LWCR and the freshly
  # signed binary fails to spawn (EX_CONFIG/78). restart_service uses this flag
  # to force a full bootout+bootstrap, which re-derives the LWCR.
  BINARY_CHANGED=1
}

# Sign the installed binary with the stable self-signed identity so granted TCC
# permissions (Full Disk Access, "data from other apps", Desktop/Downloads, …)
# survive rebuilds. macOS keys those grants to the binary's designated
# requirement (identifier + cert leaf), which is identical across rebuilds — but
# ONLY if every build is actually signed with this cert. A single unsigned build
# falls back to an ad-hoc cdhash identity that matches no grant, so macOS drops
# every toggle and re-prompts. Signing is therefore MANDATORY on macOS: if the
# cert is missing or codesign fails, abort the install rather than silently
# shipping an unsigned binary. Run scripts/codesign-setup.sh once to create the
# cert. (codesign is absent off-macOS, where this is a clean no-op.)
codesign_binary() {
  command -v codesign >/dev/null 2>&1 || return 0
  if ! security find-certificate -c "$CODESIGN_IDENTITY" >/dev/null 2>&1; then
    die "code-signing identity '$CODESIGN_IDENTITY' not found — run ./scripts/codesign-setup.sh once, then reinstall. (Shipping an unsigned binary would drop your macOS TCC grants and re-trigger permission prompts.)"
  fi
  codesign --force --sign "$CODESIGN_IDENTITY" --identifier "$LABEL" "$INSTALL_BIN" \
    || die "codesign failed — refusing to install an unsigned binary that would drop macOS TCC grants. Ensure the login keychain is unlocked and '$CODESIGN_IDENTITY' has codesign access (scripts/codesign-setup.sh)."
  # Verify the signature actually took and pins the stable designated
  # requirement — a re-signed rebuild must keep satisfying the grant macOS
  # stored, or the toggle silently turns itself off.
  codesign --verify --strict "$INSTALL_BIN" 2>/dev/null \
    || die "codesign verify failed for $INSTALL_BIN — signature did not stick; aborting to avoid dropping TCC grants."
  info "signed + verified $INSTALL_BIN (identity: $CODESIGN_IDENTITY)"
}

# --- plist ----------------------------------------------------------------
# Sets PLIST_CHANGED=1 when the on-disk plist was created or its contents
# changed, 0 when it already matched. restart_service uses this to decide
# whether a full reload (to pick up plist changes) is needed.
PLIST_CHANGED=1
# Set to 1 by deploy_binary when the binary is (re)written/(re)signed. Defaults
# to 0 so restart_service can kickstart when only a restart (no redeploy) is asked.
BINARY_CHANGED=0
render_plist() {
  [ -f "$TEMPLATE" ] || die "plist template not found: $TEMPLATE"
  mkdir -p "$(dirname "$PLIST")"
  local tmp="$PLIST.tmp.$$"
  sed -e "s|__BINARY__|$INSTALL_BIN|g" \
      -e "s|__ADDR__|$ADDR|g" \
      -e "s|__HOME__|$HOME|g" \
      "$TEMPLATE" > "$tmp" || { rm -f "$tmp"; die "failed to render plist"; }
  if [ -f "$PLIST" ] && cmp -s "$tmp" "$PLIST"; then
    rm -f "$tmp"
    PLIST_CHANGED=0
    info "plist unchanged: $PLIST"
  else
    mv -f "$tmp" "$PLIST"
    PLIST_CHANGED=1
    info "wrote $PLIST"
  fi
}

# --- launchctl ------------------------------------------------------------
service_loaded() {
  launchctl print "gui/$UID_NUM/$LABEL" >/dev/null 2>&1
}

load_service() {
  if launchctl bootstrap "gui/$UID_NUM" "$PLIST" 2>/dev/null; then
    info "service bootstrapped"
    # bootstrap registers the job but may defer the spawn as "speculative";
    # kickstart forces an immediate launch so report_health doesn't race.
    launchctl kickstart "gui/$UID_NUM/$LABEL" 2>/dev/null || true
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

# Stop (if loaded) and load again so an updated plist takes effect. Waits for
# the label to drop from the domain AND the port to be released before
# re-bootstrapping to avoid a race where bootout removes the label but the
# process is still alive holding the port.
reload_service() {
  unload_service
  local port="${ADDR##*:}" i
  for i in $(seq 1 25); do
    service_loaded || break
    sleep 0.2
  done
  # Wait for the port to be free (bootout is async — the process gets SIGTERM
  # but may outlive the domain-label removal by several seconds).
  for i in $(seq 1 25); do
    lsof -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1 || break
    sleep 0.2
  done
  load_service
}

restart_service() {
  if ! service_loaded; then
    load_service
  elif [ "${PLIST_CHANGED:-1}" -eq 1 ] || [ "${BINARY_CHANGED:-0}" -eq 1 ]; then
    # plist was (re)written, or the binary was re-signed (new cdhash) — a plain
    # kickstart re-reads neither, so fully reload to re-derive launchd's LWCR.
    reload_service
  else
    # Nothing redeployed; kickstart re-execs the existing job in place.
    launchctl kickstart -k "gui/$UID_NUM/$LABEL" || die "failed to restart service"
    info "service restarted"
  fi
}

# --- health ---------------------------------------------------------------
report_health() {
  # Normalize the probe host: a wildcard/host-less bind (":8765", "0.0.0.0",
  # "::") is fine to listen on but not to connect to — probe loopback instead.
  local host="${ADDR%:*}" port="${ADDR##*:}" url i
  case "$host" in
    ""|"0.0.0.0"|"::"|"*") host="127.0.0.1" ;;
  esac
  url="http://$host:$port/healthz"
  for i in $(seq 1 25); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then
      info "${_C_GRN}daemon healthy${_C_RST} — $url"
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

# --- Linux overrides (systemd user service) --------------------------------
# These five functions replace their launchctl counterparts above when running
# on Linux. All other helpers (build_release, deploy_binary, codesign_binary,
# report_health, check_path) are already platform-safe.
if [ "$OS_PLATFORM" = "linux" ]; then
  command -v systemctl >/dev/null 2>&1 || die "systemctl not found — systemd user session required on Linux"
  SERVICE_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/warden.service"
  TEMPLATE="$REPO_ROOT/deploy/warden.service.template"

  render_plist() {
    [ -f "$TEMPLATE" ] || die "service template not found: $TEMPLATE"
    mkdir -p "$(dirname "$SERVICE_CONFIG")"
    local tmp="$SERVICE_CONFIG.tmp.$$"
    sed -e "s|__BINARY__|$INSTALL_BIN|g" \
        -e "s|__ADDR__|$ADDR|g" \
        -e "s|__HOME__|$HOME|g" \
        "$TEMPLATE" > "$tmp" || { rm -f "$tmp"; die "failed to render service file"; }
    if [ -f "$SERVICE_CONFIG" ] && cmp -s "$tmp" "$SERVICE_CONFIG"; then
      rm -f "$tmp"
      PLIST_CHANGED=0
      info "service file unchanged: $SERVICE_CONFIG"
    else
      mv -f "$tmp" "$SERVICE_CONFIG"
      PLIST_CHANGED=1
      info "wrote $SERVICE_CONFIG"
    fi
  }

  service_loaded() {
    systemctl --user is-active --quiet warden 2>/dev/null
  }

  load_service() {
    mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
    # enable-linger keeps the user's systemd session alive after logout so the
    # daemon persists between SSH/terminal sessions, matching launchd RunAtLoad.
    loginctl enable-linger "$USER" 2>/dev/null || warn "loginctl enable-linger failed — daemon may stop on logout"
    systemctl --user daemon-reload || die "systemctl daemon-reload failed"
    if systemctl --user enable --now warden 2>/dev/null; then
      info "service enabled and started"
    else
      die "failed to enable/start warden service"
    fi
  }

  unload_service() {
    if ! service_loaded; then
      info "service not active; nothing to stop"
      return 0
    fi
    if systemctl --user disable --now warden 2>/dev/null; then
      info "service disabled and stopped"
    else
      warn "could not stop service (it may already be gone)"
    fi
  }

  reload_service() {
    systemctl --user daemon-reload || die "systemctl daemon-reload failed"
    systemctl --user restart warden || die "failed to restart warden service"
    info "service restarted"
  }

  restart_service() {
    systemctl --user daemon-reload || die "systemctl daemon-reload failed"
    if service_loaded; then
      systemctl --user restart warden || die "failed to restart warden service"
      info "service restarted"
    else
      load_service
    fi
  }
fi
