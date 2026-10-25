# Focused verification of the canonical Plan detail. Run after capture-tui-media.sh
# has seeded /tmp/wd-demo-tui:8799; this is intentionally much shorter than the
# showcase tape so navigation can be reviewed without another full GIF render.
# VHS's PNG Output is its initial frame and does not update a later Screenshot.
# Keep the recording disposable; the explicit screenshot below is the deliverable.
Output plan-recording.gif
Set Shell bash
Set FontSize 13
Set Width 2150
Set Height 1000
Set Framerate 4
Set Padding 10
Set Theme { "name": "Dracula Bright", "black": "#21222c", "red": "#ff5555", "green": "#50fa7b", "yellow": "#f1fa8c", "blue": "#bd93f9", "magenta": "#ff79c6", "cyan": "#8be9fd", "white": "#f8f8f2", "brightBlack": "#b4bcdc", "brightRed": "#ff6e6e", "brightGreen": "#69ff94", "brightYellow": "#ffffa5", "brightBlue": "#d6acff", "brightMagenta": "#ff92df", "brightCyan": "#a4ffff", "brightWhite": "#ffffff", "background": "#1e1f29", "foreground": "#f8f8f2", "selection": "#44475a", "cursor": "#f8f8f2" }

Hide
Type `eval "$(bash ../scripts/demo-seed.sh env | grep -v '^#')" && warden() { eval "$DEMO_ENV_PREFIX $DEMO_WARDEN \"\$@\""; } && export PS1='$ ' && clear`
Enter
Sleep 1s
Show
Type "warden tui"
Enter
# capture-plan.sh detects the isolated TUI and opens expanded Plan Detail via
# capture-pane assertions while this tape waits; do not encode cursor guesses.
Sleep 30s
Show
Sleep 1s
Screenshot ../public/media/tui-plan-dag.png
Sleep 2s
