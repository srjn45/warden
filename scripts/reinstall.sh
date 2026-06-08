#!/usr/bin/env bash
# Redeploy warden: rebuild (default), recopy the binary, restart the service.
# This replaces the old `make release && ./bin/warden daemon` loop.
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

info "reinstalling warden daemon"

if [ "$NO_BUILD" -eq 0 ]; then
  build_release
else
  warn "--no-build: redeploying existing bin/warden"
fi

deploy_binary
render_plist          # keep plist in sync; harmless if unchanged
restart_service       # bootstraps if not yet loaded
report_health
info "reinstall complete"
