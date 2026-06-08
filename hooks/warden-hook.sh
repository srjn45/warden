#!/usr/bin/env bash
# warden Claude Code hook. Fails soft: never blocks the agent.
# Usage (from settings.json): warden-hook.sh <EVENT_TYPE>
# The session id is the tmux session name (set when warden spawned it).
set -u

EVENT_TYPE="${1:-Unknown}"
# WARDEN_ADDR is canonical; AGENTCTL_ADDR is still honored as a fallback.
ADDR="${WARDEN_ADDR:-${AGENTCTL_ADDR:-127.0.0.1:8765}}"

# tmux session name == warden session id. Outside tmux → no-op.
SESSION="$(tmux display-message -p '#S' 2>/dev/null || true)"
[ -z "$SESSION" ] && exit 0

# Claude passes hook JSON on stdin; pull a human-readable detail if present.
DETAIL="$(cat 2>/dev/null | tr '\n' ' ' | cut -c1-200)"

curl -s -m 2 -X POST "http://${ADDR}/events" \
  -H 'Content-Type: application/json' \
  -d "$(printf '{"session":"%s","type":"%s","detail":"%s"}' \
        "$SESSION" "$EVENT_TYPE" "${DETAIL//\"/\\\"}")" \
  >/dev/null 2>&1 || true

exit 0
