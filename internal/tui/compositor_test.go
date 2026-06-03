package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
)

func TestChooseClassic(t *testing.T) {
	cases := []struct {
		name                                   string
		classicFlag, tmuxAvailable, insideTmux bool
		want                                   bool
	}{
		{"default composited", false, true, false, false},
		{"explicit --classic", true, true, false, true},
		{"no tmux falls back", false, false, false, true},
		{"inside tmux falls back", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, chooseClassic(c.classicFlag, c.tmuxAvailable, c.insideTmux))
		})
	}
}

func TestCockpitNames(t *testing.T) {
	require.Equal(t, "agentctl-tui-1234", cockpitSession(1234))
}

func TestBuildCockpitSequence(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} "+detailPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} claude"] = lifecycle.FakeResp{Out: "%1\n"}
	fr.Responses["tmux split-window -v -b -l 50% -t %1 -c /home -P -F #{pane_id} "+listPaneCmd("/bin/agentctl", "%0")] = lifecycle.FakeResp{Out: "%2\n"}

	o := cockpitOpts{session: "S", self: "/bin/agentctl", homeDir: "/home", masterCwd: "/work"}
	require.NoError(t, buildCockpit(context.Background(), fr, o))
	require.Len(t, fr.Calls, 11, "unexpected number of tmux calls")

	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", "S", "-c", "/home", "-P", "-F", "#{pane_id}", detailPlaceholderCmd()}, fr.Calls[0].Argv)
	require.Equal(t, []string{"tmux", "split-window", "-h", "-b", "-l", "40%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", "claude"}, fr.Calls[1].Argv)
	require.Equal(t, []string{"tmux", "split-window", "-v", "-b", "-l", "50%", "-t", "%1", "-c", "/home", "-P", "-F", "#{pane_id}", "/bin/agentctl tui --pane=list --detail-pane=%0"}, fr.Calls[2].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-p", "-t", "%0", "remain-on-exit", "on"}, fr.Calls[3].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "mouse", "on"}, fr.Calls[4].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Left", "select-pane", "-L"}, fr.Calls[5].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Right", "select-pane", "-R"}, fr.Calls[6].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Up", "select-pane", "-U"}, fr.Calls[7].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Down", "select-pane", "-D"}, fr.Calls[8].Argv)
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%2"}, fr.Calls[9].Argv)
	// Return-to-dashboard binding for the full-screen attach path (`a`).
	require.Equal(t, []string{"tmux", "bind-key", "Enter", "switch-client", "-l"}, fr.Calls[10].Argv)
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
	require.Equal(t, "/bin/agentctl tui --pane=list --detail-pane=%0", listPaneCmd("/bin/agentctl", "%0"))
	require.Contains(t, detailPlaceholderCmd(), "press Enter to open")
	require.True(t, strings.Contains(shquote("a b"), "'a b'"))
}
