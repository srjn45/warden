# Render the project-centric TUI cockpit screenshots against the ISOLATED demo fleet.
#
#   export DEMO_ROOT=/tmp/warden-demo DEMO_PORT=8799        # pick per capture; see below
#   bash ../scripts/demo-seed.sh up                         # seed (fails if the fixture is incomplete)
#   vhs cockpit.tape                                        # needs vhs + ttyd + ffmpeg + a chromium
#   bash optimize-gif.sh ../public/media/tui-cockpit.gif    # palette/scale/fps squeeze (<=300KB)
#   bash ../scripts/demo-seed.sh down
#
# The hidden preamble loads the demo instance's clean-env launcher, so the real
# cockpit (and every pane it re-executes) runs with HOME + --config pointing at the
# demo instance — never the recorder's real daemon, fleet, paths or token. The
# `warden` shell function below is that launcher; the visible commands read as the
# plain CLI. DEMO_ROOT/DEMO_PORT must match the `up` that seeded the instance
# (t2 TUI and t3 web captures use separate roots/ports; see demo-seed.sh).
Output ../public/media/tui-cockpit.gif
Set Shell bash
Set FontSize 13
Set Width 2150
Set Height 1000
Set Framerate 6
Set Padding 10
Set Theme { "name": "Dracula Bright", "black": "#21222c", "red": "#ff5555", "green": "#50fa7b", "yellow": "#f1fa8c", "blue": "#bd93f9", "magenta": "#ff79c6", "cyan": "#8be9fd", "white": "#f8f8f2", "brightBlack": "#b4bcdc", "brightRed": "#ff6e6e", "brightGreen": "#69ff94", "brightYellow": "#ffffa5", "brightBlue": "#d6acff", "brightMagenta": "#ff92df", "brightCyan": "#a4ffff", "brightWhite": "#ffffff", "background": "#1e1f29", "foreground": "#f8f8f2", "selection": "#44475a", "cursor": "#f8f8f2" }

# The cockpit's control pane wraps (and clips its own top) when narrower than ~105
# columns, so the capture is wide on purpose.
Hide
Type `eval "$(bash ../scripts/demo-seed.sh env | grep -v '^#')" && warden() { eval "$DEMO_ENV_PREFIX $DEMO_WARDEN \"\$@\""; } && export PS1='$ ' && clear`
Enter
Sleep 1s
Show

Type "warden tui"
Enter
Sleep 7s
# Hidden: tidy the terminal pane (focus it with the tmux prefix, clear the launch echo,
# cycle focus back to the control pane).
Hide
Ctrl+B
Type "o"
Sleep 400ms
Type "export PS1='$ ' && clear"
Enter
Sleep 400ms
Ctrl+B
Type "o"
Sleep 300ms
Ctrl+B
Type "o"
Sleep 500ms
Show

# 1. the project tree: group "platform", three projects, Plans, agents, terminals, roles —
# with a busy worker selected so the right pane shows live agent output.
Sleep 1s
Down@600ms 3
Sleep 1s
Enter
Sleep 4s
Screenshot ../public/media/cockpit.png

# Opening a worker leaves focus in the control tree, but select its tmux index
# explicitly rather than relying on focus cycling. Expand Plans then In Progress
# and open the actual Plan (not its similarly named Autopilot child).
Hide
Ctrl+B
Type ":"
Type "select-pane -t :.0"
Enter
Sleep 500ms
Show
Up@600ms 3
Sleep 1s
Enter
Sleep 1s
Down
Sleep 1s
Enter
Sleep 1s
Down
Sleep 1s
Enter
Sleep 3s
Type "t"
Sleep 1s
Screenshot ../public/media/tui-plan.png

# A busy worker and the two diagnostic panels make the full animation reflect
# the current project-centric cockpit rather than the old flat session list.
Down@600ms 8
Sleep 1s
Enter
Sleep 4s
Screenshot cockpit-agent.png
Type "b"
Sleep 3s
Screenshot cockpit-backends.png
Escape
Sleep 1s
Type "F"
Sleep 3s
Screenshot cockpit-fastbrain.png
Escape
Sleep 1s
