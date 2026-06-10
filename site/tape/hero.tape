# Render the hero GIF (deterministic, no side effects) with:
#   cd site/tape && PATH="$PWD/_demobin:$PATH" vhs hero.tape
# _demobin/warden is a stub so the recording never spawns a real agent or leaks
# the recorder's real fleet. Output mirrors the real warden format.
Output ../public/media/hero.gif
Set FontSize 18
Set Width 1200
Set Height 600
Set Padding 20
Set Theme "Dracula"

Sleep 700ms
Type "warden start 'fix the flaky nightly test'"  Sleep 500ms  Enter
Sleep 1800ms
Type "warden ls"  Sleep 500ms  Enter
Sleep 3s
