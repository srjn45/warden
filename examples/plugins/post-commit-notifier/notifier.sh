#!/bin/sh
# post-commit-notifier — a minimal warden plugin (#47) that exercises the
# JSON-over-stdio hook protocol end-to-end.
#
# warden invokes a plugin executable once per subscribed lifecycle event: it
# writes a JSON request to stdin and reads a JSON response from stdout, bounded by
# a hard timeout. The contract is fail-open — if this script is missing, slow,
# exits non-zero, or prints garbage, warden logs it and moves on; it never blocks
# or crashes the agent. So keep it fast and side-effect-light.
#
# Request shape (stdin):
#   {"protocol_version":1,"event":"post-commit",
#    "session":{"id":"dev-ab12","type":"development","repo":"/path",
#               "worktree":".worktrees/dev-ab12","branch":"dev-ab12",
#               "workdir":"/path/.worktrees/dev-ab12"},
#    "payload":{"sha":"<sha>","branch":"<branch>","committed":"true"}}
#
# Response shape (stdout) — purely advisory, recorded by warden, never gates:
#   {"protocol_version":1,"ok":true,"message":"<short note>"}
#
# This example just appends a line to a log file so you can watch hooks fire:
#   tail -f ~/.warden/plugin-notifier.log
set -eu

REQ="$(cat)"
LOG="${WARDEN_NOTIFIER_LOG:-$HOME/.warden/plugin-notifier.log}"
mkdir -p "$(dirname "$LOG")"

# Pull a couple of fields out of the request. jq if available, else a crude grep
# fallback so the example has no hard dependency.
if command -v jq >/dev/null 2>&1; then
  EVENT="$(printf '%s' "$REQ" | jq -r '.event // "?"')"
  ID="$(printf '%s' "$REQ" | jq -r '.session.id // "?"')"
  SHA="$(printf '%s' "$REQ" | jq -r '.payload.sha // "-"')"
else
  EVENT="$(printf '%s' "$REQ" | sed -n 's/.*"event":"\([^"]*\)".*/\1/p')"
  ID="$(printf '%s' "$REQ" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)"
  SHA="-"
fi

printf '%s  event=%s agent=%s sha=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$EVENT" "$ID" "$SHA" >> "$LOG"

# Always reply OK. The message is logged by warden at debug level.
printf '{"protocol_version":1,"ok":true,"message":"logged %s for %s"}' "$EVENT" "$ID"
