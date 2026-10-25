# Render the project-centric TUI cockpit screenshots against the ISOLATED demo fleet.
#
#   export DEMO_ROOT=/tmp/warden-demo DEMO_PORT=8799        # pick per capture; see below
#   bash ../scripts/demo-seed.sh up                         # seed (fails if the fixture is incomplete)
#   vhs cockpit.tape                                        # needs vhs + ttyd + ffmpeg
#   bash ../scripts/demo-seed.sh down
#
# The hidden preamble loads the demo instance's clean-env launcher, so the real
# cockpit (and every pane it re-executes) runs with HOME + --config pointing at the
# demo instance — never the recorder's real daemon, fleet, paths or token. The
# `warden` shell function below is that launcher; the visible commands read as the
# plain CLI. DEMO_ROOT/DEMO_PORT must match the `up` that seeded the instance
# (t2 TUI and t3 web captures use separate roots/ports; see demo-seed.sh).
Output cockpit-capture.gif
Set Shell bash
Set FontSize 14
Set Width 1500
Set Height 900
Set Padding 12
Set Theme "Dracula"

Hide
Type `eval "$(bash ../scripts/demo-seed.sh env | grep -v '^#')" && warden() { $DEMO_ENV_PREFIX $DEMO_WARDEN "$@"; } && export PS1='$ ' && clear`
Enter
Sleep 1s
Show

Type "warden tui"
Enter
Sleep 8s
# 1. the project tree: group "platform", three projects, Plans, agents, terminals, roles.
Screenshot cockpit-tree.png

# 2. the Autopilot run on the canonical plan — plan row opens its detail pane.
Down
Enter
Sleep 2s
Screenshot cockpit-plan.png

# 3. a worker waiting on a permission prompt: agent pane with its border tab.
Down 3
Enter
Sleep 3s
Screenshot cockpit-agent.png

# 4. agent-backend registry (tiers, default, enabled).
Type "b"
Sleep 2s
Screenshot cockpit-backends.png
Escape
Sleep 500ms

# 5. Fast-Brain telemetry + decision trace.
Type "F"
Sleep 2s
Screenshot cockpit-fastbrain.png
Escape
Sleep 500ms
