#!/usr/bin/env bash
# Squeeze a VHS GIF to a docs-friendly size: scale to a readable width, drop the
# frame rate and use a small flat palette (terminal UI is flat-colour, so
# dither-free palettes stay crisp). Usage: optimize-gif.sh file.gif [width] [fps] [colors]
set -euo pipefail
f="$1"; w="${2:-1280}"; fps="${3:-5}"; colors="${4:-48}"
tmp="$(mktemp --suffix=.gif)"
ffmpeg_bin="${FFMPEG:-ffmpeg}"
command -v "$ffmpeg_bin" >/dev/null 2>&1 || { echo "ffmpeg not found (set FFMPEG or PATH)" >&2; exit 127; }
"$ffmpeg_bin" -v error -y -i "$f" -vf "fps=$fps,scale=$w:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=$colors:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle" "$tmp"
mv "$tmp" "$f"
ls -l "$f"
