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
# Persisted bind address from the last remote install, so reinstalls/upgrades
# keep binding the same addr without re-passing WARDEN_ADDR every time. Written
# by ensure_token; removed when an install explicitly chooses loopback.
ADDR_FILE="$HOME/.warden/addr"
# WARDEN_ADDR is canonical; AGENTCTL_ADDR is honored as a fallback; then the
# persisted ADDR_FILE; finally loopback. This makes remote binding sticky: once
# a host is set up for remote access, a bare ./scripts/reinstall.sh won't
# silently revert it to 127.0.0.1.
ADDR="${WARDEN_ADDR:-${AGENTCTL_ADDR:-}}"
if [ -z "$ADDR" ] && [ -r "$ADDR_FILE" ]; then
  ADDR="$(cat "$ADDR_FILE")"
fi
ADDR="${ADDR:-127.0.0.1:8765}"
# Bearer-token file for remote (non-loopback) installs. Off the YAML config and
# out of the repo; populated by ensure_token only when ADDR is non-loopback.
TOKEN_FILE="$HOME/.warden/token.env"
# Set by ensure_token: 1 when the daemon binds a non-loopback addr (auth
# required), 0 for loopback installs which stay auth-free exactly as before.
AUTH_ENABLED=0
WARDEN_TOKEN_VALUE=""
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

# --- git hooks ------------------------------------------------------------
# Point git at the tracked .githooks/ dir so the pre-commit (gofmt/vet) and
# pre-push (make verify-fast) gates run automatically. Idempotent; relative
# path resolves per-worktree. No-op outside a git checkout (e.g. installs from
# a release tarball) — purely a developer convenience.
wire_git_hooks() {
  command -v git >/dev/null 2>&1 || return 0
  [ -d "$REPO_ROOT/.githooks" ] || return 0
  git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 0
  if git -C "$REPO_ROOT" config core.hooksPath .githooks; then
    info "git hooks wired: core.hooksPath -> .githooks (bypass any time with --no-verify)"
  else
    warn "could not set core.hooksPath; run 'make install-hooks' manually"
  fi
}

# --- build & binary -------------------------------------------------------
build_release() {
  info "building release (make release)…"
  make -C "$REPO_ROOT" release || die "make release failed"
}

deploy_binary() {
  [ -f "$REPO_ROOT/bin/warden" ] || die "bin/warden not found — build first (run without --no-build, or 'make release')"
  mkdir -p "$INSTALL_BIN_DIR"
  local _tmp="$INSTALL_BIN.tmp.$$"
  cp "$REPO_ROOT/bin/warden" "$_tmp" || die "failed to copy binary to $_tmp"
  mv -f "$_tmp" "$INSTALL_BIN" || { rm -f "$_tmp"; die "failed to install binary to $INSTALL_BIN"; }
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

# --- auth / token ---------------------------------------------------------
# True when ADDR is a loopback/host-less bind that needs no authentication.
is_loopback_addr() {
  case "${ADDR%:*}" in
    ""|"localhost"|"127.0.0.1"|"::1") return 0 ;;
    *) return 1 ;;
  esac
}

# Provision a bearer token when binding a non-loopback address. The daemon
# refuses to bind a non-loopback addr without WARDEN_TOKEN (fail-closed), so a
# remote install must supply one. The token lives only in $TOKEN_FILE
# (chmod 600) — never in the YAML config or the repo — and is generated once,
# then reused on later installs so phones/clients keep working across upgrades.
# Sets AUTH_ENABLED and WARDEN_TOKEN_VALUE, consumed by render_plist. Must run
# after deploy_binary (it shells out to the installed binary to mint the token).
ensure_token() {
  if is_loopback_addr; then
    AUTH_ENABLED=0
    # Explicit loopback install clears any sticky remote addr so the next bare
    # reinstall doesn't resurrect remote binding.
    rm -f "$ADDR_FILE"
    return 0
  fi
  AUTH_ENABLED=1
  mkdir -p "$(dirname "$TOKEN_FILE")"
  # Remember this addr so future installs stay remote without WARDEN_ADDR.
  printf '%s\n' "$ADDR" > "$ADDR_FILE"
  [ -f "$TOKEN_FILE" ] && WARDEN_TOKEN_VALUE="$(sed -n 's/^WARDEN_TOKEN=//p' "$TOKEN_FILE")"
  if [ -n "$WARDEN_TOKEN_VALUE" ]; then
    info "reusing existing remote token: $TOKEN_FILE"
  else
    WARDEN_TOKEN_VALUE="$("$INSTALL_BIN" token generate)" || die "failed to generate bearer token"
    ( umask 077; printf 'WARDEN_TOKEN=%s\n' "$WARDEN_TOKEN_VALUE" > "$TOKEN_FILE" ) \
      || die "failed to write token file: $TOKEN_FILE"
    info "generated remote bearer token -> $TOKEN_FILE"
  fi
  chmod 600 "$TOKEN_FILE"
}

# Print the token + usage hint at the end of a remote install. No-op for
# loopback installs.
auth_notice() {
  [ "${AUTH_ENABLED:-0}" -eq 1 ] || return 0
  info "remote access enabled on ${_C_GRN}$ADDR${_C_RST} — clients authenticate with this bearer token:"
  printf '    %s\n' "$WARDEN_TOKEN_VALUE"
  printf '    stored in %s (chmod 600)\n' "$TOKEN_FILE"
  printf '    • web/phone: open http://<this-host>:%s and paste the token\n' "${ADDR##*:}"
  printf '    • CLI/TUI on this host: export WARDEN_TOKEN from that file, e.g. add to your shell rc:\n'
  printf '        [ -r %s ] && export WARDEN_TOKEN="$(sed -n '"'"'s/^WARDEN_TOKEN=//p'"'"' %s)"\n' "$TOKEN_FILE" "$TOKEN_FILE"
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
  # launchd has no EnvironmentFile equivalent, so the token is inlined into the
  # plist's EnvironmentVariables; the plist is chmod 600'd below to keep it
  # readable only by the user. Loopback installs delete the placeholder line.
  local token_sed
  if [ "${AUTH_ENABLED:-0}" -eq 1 ]; then
    token_sed="s|__TOKENENV__|<key>WARDEN_TOKEN</key><string>$WARDEN_TOKEN_VALUE</string>|"
  else
    token_sed="/__TOKENENV__/d"
  fi
  sed -e "s|__BINARY__|$INSTALL_BIN|g" \
      -e "s|__ADDR__|$ADDR|g" \
      -e "s|__HOME__|$HOME|g" \
      -e "$token_sed" \
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
  # Restrict the plist when it carries an inlined token (it would otherwise be
  # world-readable). Explicit if — a bare `[ ] && …` would return 1 on loopback
  # and trip `set -e` in the caller.
  if [ "${AUTH_ENABLED:-0}" -eq 1 ]; then chmod 600 "$PLIST"; fi
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
    # Non-loopback installs load the bearer token from $TOKEN_FILE via
    # EnvironmentFile; loopback installs delete the placeholder line so the unit
    # is byte-for-byte identical to the historical auth-free service.
    local envfile_sed
    if [ "${AUTH_ENABLED:-0}" -eq 1 ]; then
      envfile_sed="s|__ENVFILE__|EnvironmentFile=$TOKEN_FILE|"
    else
      envfile_sed="/__ENVFILE__/d"
    fi
    sed -e "s|__BINARY__|$INSTALL_BIN|g" \
        -e "s|__ADDR__|$ADDR|g" \
        -e "s|__HOME__|$HOME|g" \
        -e "$envfile_sed" \
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
