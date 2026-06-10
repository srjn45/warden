# Render the TUI cockpit screenshot (public/media/cockpit.png).
#
# Prereqs: run site/scripts/demo-seed.sh, then start the demo daemon on
# 127.0.0.1:8799 (see that script's printed next-steps). Then, from site/tape:
#   PATH="$PWD/_demobin:$PATH" vhs cockpit.tape
#
# _demobin/claude stubs the master pane so the recording starts no real Claude
# session. The inline `env` on the launch line isolates tmux onto the demo's
# server (TMUX_TMPDIR) and unsets TMUX so `warden tui` can build its panes even
# when this tape is rendered from inside a tmux session.
Output cockpit-capture.gif
Set FontSize 14
Set Width 1500
Set Height 900
Set Padding 12
Set Theme "Dracula"

Sleep 800ms
Type "env -u TMUX HOME=/tmp/work WARDEN_APPROVALS=1 WARDEN_ADDR=127.0.0.1:8799 WARDEN_DATA_DIR=/tmp/warden-demo TMUX_TMPDIR=/tmp/warden-demo-tmux PATH=$PWD/_demobin:/usr/local/bin:/usr/bin:/bin /Users/srajan.pathak/workspace/personal/warden/bin/warden tui"
Enter
Sleep 4500ms
# Walk the cursor down to a working agent so the detail pane shows live tool
# activity (Read/Update/Bash + "esc to interrupt") rather than a quiet idle one.
Down@400ms 4
Sleep 600ms
Enter
Sleep 3500ms
Screenshot cockpit.png
Sleep 400ms
