package tui

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

// listPanesKey is the FakeRunner key for the health-check probe EnsureWebCockpit
// runs against an existing session.
const listPanesKey = "tmux list-panes -t " + WebCockpitSession + " -F #{pane_at_top}#{pane_at_left} #{pane_current_command}"

// buildResponses returns the canned pane-id replies buildCockpit needs so a
// rebuild path (kill → new-session → split-window …) succeeds. self is the warden
// binary path, launchCwd the terminal/control pane cwd.
func buildResponses(t *testing.T, self, launchCwd string) map[string]lifecycle.FakeResp {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	return map[string]lifecycle.FakeResp{
		"tmux new-session -d -s " + WebCockpitSession + " -c " + home + " -P -F #{pane_id} " + agentPlaceholderCmd():     {Out: "%0\n"},
		"tmux split-window -h -b -l 40% -t %0 -c " + launchCwd + " -P -F #{pane_id} " + terminalPlaceholderCmd():         {Out: "%1\n"},
		"tmux split-window -v -b -l 50% -t %1 -c " + launchCwd + " -P -F #{pane_id} " + controlPaneCmd(self, "%0", "%1"): {Out: "%2\n"},
	}
}

// argvSeen reports whether the fake recorded a call whose argv starts with want.
func argvSeen(fr *lifecycle.FakeRunner, want ...string) bool {
	for _, c := range fr.Calls {
		if len(c.Argv) < len(want) {
			continue
		}
		match := true
		for i, w := range want {
			if c.Argv[i] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// A healthy existing cockpit (three panes, top-left running the warden bloom app)
// is reused untouched: has-session probe + one health-check list-panes, no
// kill/build.
func TestEnsureWebCockpitReusesHealthy(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		// has-session unmatched → (\"\", nil) = success = exists.
		listPanesKey: {Out: "11 warden\n01 zsh\n10 sleep\n"},
	}}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", false)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	require.Len(t, fr.Calls, 2, "a healthy cockpit must be probed then reused, not rebuilt")
	require.Equal(t, []string{"tmux", "has-session", "-t", WebCockpitSession}, fr.Calls[0].Argv)
	require.Equal(t, []string{"tmux", "list-panes", "-t", WebCockpitSession, "-F", "#{pane_at_top}#{pane_at_left} #{pane_current_command}"}, fr.Calls[1].Argv)
	require.False(t, argvSeen(fr, "tmux", "kill-session"), "healthy cockpit must not be killed")
	require.False(t, argvSeen(fr, "tmux", "new-session"), "healthy cockpit must not be rebuilt")
}

// A wedged cockpit whose top-left control pane fell back to a bare shell (wrong
// command) is torn down and rebuilt.
func TestEnsureWebCockpitRebuildsWrongCommand(t *testing.T) {
	resp := buildResponses(t, "/bin/warden", "/work")
	resp[listPanesKey] = lifecycle.FakeResp{Out: "11 zsh\n01 zsh\n10 sleep\n"} // control pane is a shell, not warden
	fr := &lifecycle.FakeRunner{Responses: resp}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", false)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	require.True(t, argvSeen(fr, "tmux", "kill-session", "-t", WebCockpitSession), "wedged cockpit must be killed")
	require.True(t, argvSeen(fr, "tmux", "new-session", "-d", "-s", WebCockpitSession), "wedged cockpit must be rebuilt")
}

// A partial cockpit (wrong pane count — e.g. a daemon crash mid-buildCockpit) is
// torn down and rebuilt.
func TestEnsureWebCockpitRebuildsWrongPaneCount(t *testing.T) {
	resp := buildResponses(t, "/bin/warden", "/work")
	resp[listPanesKey] = lifecycle.FakeResp{Out: "11 warden\n01 zsh\n"} // only two panes
	fr := &lifecycle.FakeRunner{Responses: resp}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", false)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	require.True(t, argvSeen(fr, "tmux", "kill-session", "-t", WebCockpitSession), "partial cockpit must be killed")
	require.True(t, argvSeen(fr, "tmux", "new-session", "-d", "-s", WebCockpitSession), "partial cockpit must be rebuilt")
}

// forceRebuild (the `warden tui --rebuild-web-cockpit` escape hatch) kills and
// rebuilds even a cockpit that would otherwise pass the health check — the
// list-panes probe is skipped entirely.
func TestEnsureWebCockpitForceRebuild(t *testing.T) {
	resp := buildResponses(t, "/bin/warden", "/work")
	// A perfectly healthy layout — force must ignore it.
	resp[listPanesKey] = lifecycle.FakeResp{Out: "11 warden\n01 zsh\n10 sleep\n"}
	fr := &lifecycle.FakeRunner{Responses: resp}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", true)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	require.False(t, argvSeen(fr, "tmux", "list-panes"), "forced rebuild must skip the health probe")
	require.True(t, argvSeen(fr, "tmux", "kill-session", "-t", WebCockpitSession), "forced rebuild must kill first")
	require.True(t, argvSeen(fr, "tmux", "new-session", "-d", "-s", WebCockpitSession), "forced rebuild must rebuild")
}

// When the cockpit session is absent, EnsureWebCockpit builds it (under the
// stable web session name) and returns that name — no health probe, no kill.
func TestEnsureWebCockpitBuildsWhenAbsent(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	resp := buildResponses(t, "/bin/warden", "/work")
	resp["tmux has-session -t "+WebCockpitSession] = lifecycle.FakeResp{Err: errors.New("no session")}
	fr := &lifecycle.FakeRunner{Responses: resp}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", false)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	// Probed first, then built under the web session name — no health probe or kill
	// when there was no session to begin with.
	require.Equal(t, []string{"tmux", "has-session", "-t", WebCockpitSession}, fr.Calls[0].Argv)
	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", WebCockpitSession, "-c", home, "-P", "-F", "#{pane_id}", agentPlaceholderCmd()}, fr.Calls[1].Argv)
	require.False(t, argvSeen(fr, "tmux", "list-panes"), "absent cockpit needs no health probe")
	require.False(t, argvSeen(fr, "tmux", "kill-session"), "absent cockpit needs no teardown")
}

// cockpitHealthy's judgement across representative layouts.
func TestCockpitHealthy(t *testing.T) {
	cases := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"healthy", "11 warden\n01 zsh\n10 sleep\n", nil, true},
		{"control pane is a shell", "11 zsh\n01 zsh\n10 sleep\n", nil, false},
		{"too few panes", "11 warden\n01 zsh\n", nil, false},
		{"too many panes", "11 warden\n01 zsh\n10 sleep\n10 sleep\n", nil, false},
		{"no top-left pane", "01 warden\n10 zsh\n10 sleep\n", nil, false},
		{"tmux error", "", errors.New("no server"), false},
		{"empty output", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
				listPanesKey: {Out: tc.out, Err: tc.err},
			}}
			got := cockpitHealthy(context.Background(), fr, WebCockpitSession, "/bin/warden")
			require.Equal(t, tc.want, got)
		})
	}
}
