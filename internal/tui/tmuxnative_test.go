package tui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

func TestInsideTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,1234,0")
	require.True(t, InsideTmux())
	t.Setenv("TMUX", "")
	require.False(t, InsideTmux())
}

func TestControlPaneNativeCmd(t *testing.T) {
	// The native control pane mirrors the classic one but adds --kill-window so `q`
	// tears down only the cockpit window, not the user's whole tmux session.
	require.Equal(t, "/bin/warden tui --pane=control --agent-pane=%0 --kill-window",
		controlPaneNativeCmd("/bin/warden", "%0"))
}

func TestNativeAgentPlaceholderCmd(t *testing.T) {
	s := nativeAgentPlaceholderCmd()
	require.Contains(t, s, "press Enter to open")
	// The native placeholder must NOT carry the classic mouse/shift-drag tip: the
	// native cockpit never forces `mouse on`, so that tip would be misleading.
	require.NotContains(t, s, "shift")
	require.Contains(t, s, "Ctrl-b L")
}

func TestBuildTmuxNativeCockpitSequence(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-window -n warden-cockpit -c /work -P -F #{pane_id} "+nativeAgentPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} "+controlPaneNativeCmd("/bin/warden", "%0")] = lifecycle.FakeResp{Out: "%1\n"}

	o := tmuxNativeOpts{self: "/bin/warden", cwd: "/work"}
	require.NoError(t, buildTmuxNativeCockpit(context.Background(), fr, o))

	// 1. Agent pane created via new-window (a window in the *current* session —
	//    no new-session, so no nesting and nothing to attach to).
	require.Equal(t, []string{"tmux", "new-window", "-n", "warden-cockpit", "-c", "/work", "-P", "-F", "#{pane_id}", nativeAgentPlaceholderCmd()}, fr.Calls[0].Argv)
	// 2. Control pane split to the left, handed the agent pane's id.
	require.Equal(t, []string{"tmux", "split-window", "-h", "-b", "-l", "40%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", controlPaneNativeCmd("/bin/warden", "%0")}, fr.Calls[1].Argv)
	// 3. remain-on-exit is pane-scoped (-p) so it never touches the user's session.
	require.Equal(t, []string{"tmux", "set-option", "-p", "-t", "%0", "remain-on-exit", "on"}, fr.Calls[2].Argv)
	// 4. Extended-keys passthrough (best-effort) — the EnsureExtendedKeys calls.
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Enter", "send-keys", "C-j"}, fr.Calls[3].Argv)
	// 5. Focus lands on the control pane.
	last := fr.Calls[len(fr.Calls)-1].Argv
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%1"}, last)

	// The native build must NEVER create a session or install global bindings /
	// session-scoped options that would clobber the user's own tmux config.
	for _, c := range fr.Calls {
		require.NotEqual(t, "new-session", c.Argv[1], "native cockpit must not create a session")
		if c.Argv[1] == "bind-key" {
			// The only bind-key allowed is the pane-local M-Enter newline fallback
			// from EnsureExtendedKeys (send-keys), never a global nav binding.
			require.Contains(t, c.Argv, "send-keys", "unexpected global key binding in native cockpit")
		}
		if c.Argv[1] == "set-option" {
			// No session-scoped set-option (mouse, status-right, …): only the
			// pane-scoped remain-on-exit and server-scoped extended-keys are allowed.
			require.NotContains(t, []string{"mouse", "status-right"}, lastNonFlag(c.Argv))
		}
	}
}

// lastNonFlag returns the option name a set-option call targets (its last arg
// before the value), used to assert we set no session-scoped options.
func lastNonFlag(argv []string) string {
	if len(argv) < 2 {
		return ""
	}
	return argv[len(argv)-1]
}

func TestKillCockpitArgs(t *testing.T) {
	// Classic cockpit owns its session: drop the Enter override AND the <prefix>
	// t/a/T/A rotation fallbacks (server-global bindings), then kill the session.
	// (p/P are gone — the M-p pipeline-agent rotation is retired in C2.)
	require.Equal(t, [][]string{
		{"unbind-key", "Enter"},
		{"unbind-key", "t"}, {"unbind-key", "a"},
		{"unbind-key", "T"}, {"unbind-key", "A"},
		{"kill-session"},
	}, killCockpitArgs(false))
	// Native cockpit lives in the user's session: kill only our window, and leave
	// the user's key bindings untouched (no unbind-key).
	require.Equal(t, [][]string{{"kill-window"}}, killCockpitArgs(true))
}
