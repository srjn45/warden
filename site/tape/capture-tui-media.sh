#!/usr/bin/env bash
# Run this from the repository root in one persistent shell. It owns only t2's
# isolated fixture; t3 uses a different root and port.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEMO_ROOT=/tmp/wd-demo-tui
DEMO_PORT=8799
BIN=/tmp/warden-site-media-integration/bin/warden
SCRATCH=/tmp/wd-t2-render
TOOLS_DIR="${WD_T2_TOOLS_DIR:-$HOME/.cache/wd-t2/bin}"
mkdir -p "$(dirname "$BIN")" "$SCRATCH/tmp" "$SCRATCH/home" "$SCRATCH/config"

# The capture must exercise the current integration source, never a user daemon.
if [[ ! -x "$BIN" ]]; then
  (cd "$ROOT" && go build -o "$BIN" ./cmd/warden)
fi
export WARDEN_BIN="$BIN" DEMO_ROOT DEMO_PORT
cd "$ROOT"
bash site/scripts/demo-seed.sh up
bash site/scripts/demo-seed.sh verify
# Prove the capture endpoint is the isolated instance and every CLI call has --config.
"$BIN" --config "$DEMO_ROOT/home/.warden/config.yaml" ls --json >/dev/null

cd "$ROOT/site/tape"
export PATH="$TOOLS_DIR:$PATH"
if [ -x "$TOOLS_DIR/ffmpeg" ]; then export FFMPEG="$TOOLS_DIR/ffmpeg"; fi
export TMPDIR="$SCRATCH/tmp" HOME="$SCRATCH/home" XDG_CONFIG_HOME="$SCRATCH/config" XDG_CACHE_HOME="$SCRATCH/cache"
mkdir -p "$XDG_CACHE_HOME"
vhs cockpit.tape
vhs hero.tape
"$ROOT/site/tape/optimize-gif.sh" "$ROOT/site/public/media/tui-cockpit.gif"
"$ROOT/site/tape/optimize-gif.sh" "$ROOT/site/public/media/hero.gif" 1100 4 32
