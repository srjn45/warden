package tui

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

func TestCockpitNames(t *testing.T) {
	require.Equal(t, "warden-tui-1234", cockpitSession(1234))
}

func TestBuildCockpitSequence(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} "+agentPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} "+terminalPlaceholderCmd()] = lifecycle.FakeResp{Out: "%1\n"}
	fr.Responses["tmux split-window -v -b -l 50% -t %1 -c /work -P -F #{pane_id} "+controlPaneCmd("/bin/warden", "%0", "%1")] = lifecycle.FakeResp{Out: "%2\n"}

	o := cockpitOpts{session: "S", self: "/bin/warden", homeDir: "/home", launchCwd: "/work"}
	require.NoError(t, buildCockpit(context.Background(), fr, o))
	require.Len(t, fr.Calls, 17, "unexpected number of tmux calls")

	// Panes are created right-to-left: agent (fills window) → terminal (left) → control.
	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", "S", "-c", "/home", "-P", "-F", "#{pane_id}", agentPlaceholderCmd()}, fr.Calls[0].Argv)
	require.Equal(t, []string{"tmux", "split-window", "-h", "-b", "-l", "40%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", terminalPlaceholderCmd()}, fr.Calls[1].Argv)
	require.Equal(t, []string{"tmux", "split-window", "-v", "-b", "-l", "50%", "-t", "%1", "-c", "/work", "-P", "-F", "#{pane_id}", "/bin/warden tui --pane=control --agent-pane=%0 --terminal-pane=%1"}, fr.Calls[2].Argv)
	// Both viewport panes keep their layout on attach-exit (terminal first, then agent).
	require.Equal(t, []string{"tmux", "set-option", "-p", "-t", "%1", "remain-on-exit", "on"}, fr.Calls[3].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-p", "-t", "%0", "remain-on-exit", "on"}, fr.Calls[4].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "mouse", "on"}, fr.Calls[5].Argv)
	// Permanent status-line reminder of the Shift-to-select trick (mouse drives tmux).
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "status-right", "#[fg=yellow]shift+drag = select/copy#[default]  %H:%M "}, fr.Calls[6].Argv)
	// Alt+Enter fallback newline key (for terminals that can't report Shift+Enter)…
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Enter", "send-keys", "C-j"}, fr.Calls[7].Argv)
	// …plus extended-keys passthrough so Shift+Enter reaches the agent as a newline.
	require.Equal(t, []string{"tmux", "set-option", "-s", "extended-keys", "on"}, fr.Calls[8].Argv)
	require.Equal(t, []string{"tmux", "show-options", "-s", "-v", "terminal-features"}, fr.Calls[9].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-sa", "terminal-features", "*:extkeys"}, fr.Calls[10].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Left", "select-pane", "-L"}, fr.Calls[11].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Right", "select-pane", "-R"}, fr.Calls[12].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Up", "select-pane", "-U"}, fr.Calls[13].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Down", "select-pane", "-D"}, fr.Calls[14].Argv)
	// No M-t binding any more — the old shell-toggle is gone; M-t is freed for the
	// stage-5 terminal rotation.
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%2"}, fr.Calls[15].Argv)
	// Return-to-dashboard binding for the full-screen attach path (`a`).
	require.Equal(t, []string{"tmux", "bind-key", "Enter", "switch-client", "-l"}, fr.Calls[16].Argv)
}

func TestNoMasterToggleBinding(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} "+agentPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} "+terminalPlaceholderCmd()] = lifecycle.FakeResp{Out: "%1\n"}
	fr.Responses["tmux split-window -v -b -l 50% -t %1 -c /work -P -F #{pane_id} "+controlPaneCmd("/bin/warden", "%0", "%1")] = lifecycle.FakeResp{Out: "%2\n"}
	require.NoError(t, buildCockpit(context.Background(), fr, cockpitOpts{session: "S", self: "/bin/warden", homeDir: "/home", launchCwd: "/work"}))
	for _, c := range fr.Calls {
		require.NotContains(t, c.Argv, "M-t", "the M-t shell-toggle binding must be gone")
	}
}

func TestCleanStaleCockpits(t *testing.T) {
	alive := cockpitSession(os.Getpid()) // this process is alive — must survive
	dead := cockpitSession(2147483646)   // not a live pid — must be killed
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"tmux list-sessions -F #{session_name}": {Out: alive + "\n" + dead + "\nmy-own-session\n"},
	}}
	cleanStaleCockpits(fr)

	var killed []string
	for _, c := range fr.Calls {
		if len(c.Argv) >= 4 && c.Argv[1] == "kill-session" {
			killed = append(killed, c.Argv[3])
		}
	}
	require.Equal(t, []string{dead}, killed, "only the dead-pid cockpit is killed; live cockpit and user sessions are left alone")
}

func TestPaneCommandStrings(t *testing.T) {
	require.Equal(t, "/bin/warden tui --pane=control --agent-pane=%0 --terminal-pane=%1", controlPaneCmd("/bin/warden", "%0", "%1"))
	require.Contains(t, agentPlaceholderCmd(), "press Enter to open")
	require.Contains(t, terminalPlaceholderCmd(), "terminal")
}
