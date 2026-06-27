package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

func TestCockpitNames(t *testing.T) {
	require.Equal(t, "warden-tui-1234", cockpitSession(1234))
}

func TestBuildCockpitSequence(t *testing.T) {
	// Determine the expected shell command (same logic as buildCockpit)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} "+detailPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} "+shell] = lifecycle.FakeResp{Out: "%1\n"}
	fr.Responses["tmux split-window -v -b -l 50% -t %1 -c /work -P -F #{pane_id} "+listPaneCmd("/bin/warden", "%0")] = lifecycle.FakeResp{Out: "%2\n"}

	o := cockpitOpts{session: "S", self: "/bin/warden", homeDir: "/home", masterCwd: "/work"}
	require.NoError(t, buildCockpit(context.Background(), fr, o))
	require.Len(t, fr.Calls, 17, "unexpected number of tmux calls")

	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", "S", "-c", "/home", "-P", "-F", "#{pane_id}", detailPlaceholderCmd()}, fr.Calls[0].Argv)
	require.Equal(t, []string{"tmux", "split-window", "-h", "-b", "-l", "40%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", shell}, fr.Calls[1].Argv)
	require.Equal(t, []string{"tmux", "split-window", "-v", "-b", "-l", "50%", "-t", "%1", "-c", "/work", "-P", "-F", "#{pane_id}", "/bin/warden tui --pane=list --detail-pane=%0"}, fr.Calls[2].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-p", "-t", "%0", "remain-on-exit", "on"}, fr.Calls[3].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "mouse", "on"}, fr.Calls[4].Argv)
	// Permanent status-line reminder of the Shift-to-select trick (mouse drives tmux).
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "status-right", "#[fg=yellow]shift+drag = select/copy#[default]  %H:%M "}, fr.Calls[5].Argv)
	// Alt+Enter fallback newline key (for terminals that can't report Shift+Enter)…
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Enter", "send-keys", "C-j"}, fr.Calls[6].Argv)
	// …plus extended-keys passthrough so Shift+Enter reaches Claude as a newline.
	require.Equal(t, []string{"tmux", "set-option", "-s", "extended-keys", "on"}, fr.Calls[7].Argv)
	require.Equal(t, []string{"tmux", "show-options", "-s", "-v", "terminal-features"}, fr.Calls[8].Argv)
	require.Equal(t, []string{"tmux", "set-option", "-sa", "terminal-features", "*:extkeys"}, fr.Calls[9].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Left", "select-pane", "-L"}, fr.Calls[10].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Right", "select-pane", "-R"}, fr.Calls[11].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Up", "select-pane", "-U"}, fr.Calls[12].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Down", "select-pane", "-D"}, fr.Calls[13].Argv)
	// M-t toggles the bottom-left master pane between Claude and a shell.
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-t", "run-shell", "-b", shellToggleScript("S", "%1", "/work")}, fr.Calls[14].Argv)
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%2"}, fr.Calls[15].Argv)
	// Return-to-dashboard binding for the full-screen attach path (`a`).
	require.Equal(t, []string{"tmux", "bind-key", "Enter", "switch-client", "-l"}, fr.Calls[16].Argv)
}

func TestBuildCockpitReplMasterPane(t *testing.T) {
	// The repl flavor runs `wd repl` in the master pane instead of $SHELL.
	replCmd := "/bin/warden repl"
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} "+detailPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} "+replCmd] = lifecycle.FakeResp{Out: "%1\n"}
	fr.Responses["tmux split-window -v -b -l 50% -t %1 -c /work -P -F #{pane_id} "+listPaneCmd("/bin/warden", "%0")] = lifecycle.FakeResp{Out: "%2\n"}

	o := cockpitOpts{session: "S", self: "/bin/warden", homeDir: "/home", masterCwd: "/work", useRepl: true}
	require.NoError(t, buildCockpit(context.Background(), fr, o))
	require.Equal(t, []string{"tmux", "split-window", "-h", "-b", "-l", "40%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", replCmd}, fr.Calls[1].Argv)
}

func TestMasterPaneCmd(t *testing.T) {
	require.Equal(t, "/bin/warden repl", masterPaneCmd("/bin/warden", true))
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	require.Equal(t, shell, masterPaneCmd("/bin/warden", false))
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

func TestShellToggleScript(t *testing.T) {
	s := shellToggleScript("S", "%1", "/work")
	// Tracks the shell pane in a session user-option so the toggle survives exit.
	require.Contains(t, s, "@warden_shell_pane")
	// Lazily creates the shell in a hidden holding window with the user's $SHELL.
	require.Contains(t, s, "new-window -d -t S -n warden-shell -c '/work'")
	require.Contains(t, s, `"${SHELL:-/bin/sh}"`)
	// Exited shells are kept as [exited] then respawned, not orphaned.
	require.Contains(t, s, "remain-on-exit on")
	require.Contains(t, s, "respawn-pane")
	// Swaps the shell with the master pane and focuses whatever lands in the slot.
	require.Contains(t, s, `swap-pane -s "$sp" -t %1`)
	require.Contains(t, s, "select-pane -t '{bottom-left}'")
}

func TestPaneCommandStrings(t *testing.T) {
	require.Equal(t, "/bin/warden tui --pane=list --detail-pane=%0", listPaneCmd("/bin/warden", "%0"))
	require.Contains(t, detailPlaceholderCmd(), "press Enter to open")
	require.True(t, strings.Contains(shquote("a b"), "'a b'"))
}
