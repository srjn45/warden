# Render the hero GIF against the ISOLATED demo fleet (see cockpit.tape for the
# env contract; the hidden preamble defines a `warden` launcher bound to it):
#   bash ../scripts/demo-seed.sh up && vhs hero.tape && bash ../scripts/demo-seed.sh down
Output ../public/media/hero.gif
Set Shell bash
Set FontSize 18
Set Width 1200
Set Height 600
Set Padding 20
Set Theme "Dracula"

Hide
Type `eval "$(bash ../scripts/demo-seed.sh env | grep -v '^#')" && warden() { $DEMO_ENV_PREFIX $DEMO_WARDEN "$@"; } && export PS1='$ ' && clear`
Enter
Sleep 1s
Show

Sleep 700ms
Type "warden ls"  Sleep 500ms  Enter
Sleep 3s
Type "warden usage"  Sleep 500ms  Enter
Sleep 3s
