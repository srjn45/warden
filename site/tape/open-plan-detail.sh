#!/usr/bin/env bash
# Deterministically open expanded canonical Plan Detail in an existing demo TUI.
# Usage: DEMO_ROOT=/tmp/wd-demo-tui bash site/tape/open-plan-detail.sh warden-tui-104852
set -euo pipefail
root="${DEMO_ROOT:-/tmp/wd-demo-tui}"
session="${1:?tmux session required}"
tmux=(env -u TMUX TMUX_TMPDIR="$root/tmux" tmux)
pane="$(${tmux[@]} list-panes -t "$session" -F '#{pane_index}:#{pane_id}' | awk -F: '$1==0 {print $2}')"
[ -n "$pane" ] || { echo "control pane index 0 not found" >&2; exit 1; }
${tmux[@]} select-pane -t "$pane"
view() { ${tmux[@]} capture-pane -p -t "$pane"; }
key() { ${tmux[@]} send-keys -t "$pane" "$@"; sleep 0.4; }
seek() { # direction, regex
  local dir="$1" re="$2" i
  for i in $(seq 1 40); do view | grep -Eq "$re" && return 0; key "$dir"; done
  echo "did not find selected row: $re" >&2; view >&2; return 1
}
canonical_selected() { view | awk '/›/ && /SSO rollout/ && !/autopilot\// { ok=1 } END { exit !ok }'; }
seek_canonical() {
  local i
  for i in $(seq 1 40); do canonical_selected && return 0; key Down; done
  echo "did not find selected canonical Plan" >&2; view >&2; return 1
}

# Fresh TUI starts at the first project. Clamp there (rather than inheriting a
# stale worker cursor), ensure the project is expanded, then seek Plans below.
${tmux[@]} send-keys -t "$pane" -N 100 Up
sleep 0.5
key Right
# Headers and canonical plans use distinct rows; this avoids Autopilot rows.
seek Down '›.*Plans'
key Right
seek Down '›.*In Progress'
key Right
seek_canonical
key Enter
key t
# Zoom is a tmux client operation, not input for the TUI program.
${tmux[@]} select-pane -t "$pane"
${tmux[@]} resize-pane -Z -t "$pane"
sleep 1
view | grep -q 'Plan Detail' || { echo 'Plan Detail did not open' >&2; exit 1; }
view | grep -q 'after:' || { echo 'expanded after edges not visible' >&2; view >&2; exit 1; }
echo "$pane verified: Plan Detail with after edges"
