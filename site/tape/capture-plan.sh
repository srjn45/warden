#!/usr/bin/env bash
# Focused, reproducible Plan-DAG capture; run from repository root.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEMO_ROOT=/tmp/wd-demo-tui
DEMO_PORT=8799
BIN=/tmp/warden-site-media-integration/bin/warden
SCRATCH=/tmp/wd-t2-render
TOOLS_DIR="${WD_T2_TOOLS_DIR:-$HOME/.cache/wd-t2/bin}"
mkdir -p "$(dirname "$BIN")" "$SCRATCH/tmp" "$SCRATCH/home" "$SCRATCH/config" "$SCRATCH/cache"
if [[ ! -x "$BIN" ]]; then
  (cd "$ROOT" && go build -o "$BIN" ./cmd/warden)
fi
export WARDEN_BIN="$BIN" DEMO_ROOT DEMO_PORT
cd "$ROOT"
bash site/scripts/demo-seed.sh up
bash site/scripts/demo-seed.sh verify
"$BIN" --config "$DEMO_ROOT/home/.warden/config.yaml" ls --json >/dev/null
cd "$ROOT/site/tape"
export PATH="$TOOLS_DIR:$PATH"
export TMPDIR="$SCRATCH/tmp" HOME="$SCRATCH/home" XDG_CONFIG_HOME="$SCRATCH/config" XDG_CACHE_HOME="$SCRATCH/cache"
vhs plan.tape &
vhs_pid=$!
trap 'kill "$vhs_pid" 2>/dev/null || true' ERR
# The tape pauses after `warden tui`. Locate its isolated tmux session and
# prove the exact control-pane state before VHS reaches Screenshot.
for _ in $(seq 1 60); do
  session="$(env -u TMUX TMUX_TMPDIR="$DEMO_ROOT/tmux" tmux ls -F '#{session_name}' 2>/dev/null | awk '/^warden-tui-/{print; exit}')"
  [ -n "${session:-}" ] && break
  sleep 0.5
done
[ -n "${session:-}" ] || { kill "$vhs_pid" 2>/dev/null || true; echo "TUI session not found" >&2; exit 1; }
bash "$ROOT/site/tape/open-plan-detail.sh" "$session"
wait "$vhs_pid"
trap - ERR
