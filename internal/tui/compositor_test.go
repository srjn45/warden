package tui

import (
	"context"
	"os"
	"path/filepath"
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
	require.Equal(t, filepath.Join("/run/agentctl", "tui-1234"), cockpitStateDir("/run/agentctl", 1234))
}

func TestBuildCockpitSequence(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	// Make each pane-creating tmux call return a distinct pane id.
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} /bin/agentctl tui --pane=list --state-dir=/st"] = lifecycle.FakeResp{Out: "%0\n"}

	o := cockpitOpts{session: "S", self: "/bin/agentctl", stateDir: "/st", homeDir: "/home", masterCwd: "/work"}
	err := buildCockpit(context.Background(), fr, o)
	require.NoError(t, err)

	// 1) session created with the list pane command
	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", "S", "-c", "/home", "-P", "-F", "#{pane_id}", "/bin/agentctl tui --pane=list --state-dir=/st"}, fr.Calls[0].Argv)
	// 2) detail pane: split the list pane horizontally, 60% to the new right pane
	require.Equal(t, []string{"tmux", "split-window", "-h", "-l", "60%", "-t", "%0", "-c", "/home", "-P", "-F", "#{pane_id}", "/bin/agentctl tui --pane=detail --state-dir=/st"}, fr.Calls[1].Argv)
	// 3) master pane: split the list pane vertically, 50%, running claude in the work dir
	require.Equal(t, []string{"tmux", "split-window", "-v", "-l", "50%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", "claude"}, fr.Calls[2].Argv)
	// 4) mouse on + focus the list pane
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "mouse", "on"}, fr.Calls[3].Argv)
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%0"}, fr.Calls[4].Argv)
}

func TestPaneCommandStrings(t *testing.T) {
	require.Equal(t, "/bin/agentctl tui --pane=list --state-dir=/st", listPaneCmd("/bin/agentctl", "/st"))
	require.Equal(t, "/bin/agentctl tui --pane=detail --state-dir=/st", detailPaneCmd("/bin/agentctl", "/st"))
	// paths with spaces are single-quoted so tmux's `sh -c` keeps them intact
	require.True(t, strings.Contains(shquote("a b"), "'a b'"))
}

func TestCockpitBaseDirPrefersXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	require.Equal(t, filepath.Join("/run/user/1000", "agentctl"), cockpitBaseDir())
	t.Setenv("XDG_RUNTIME_DIR", "")
	require.Equal(t, filepath.Join(os.TempDir(), "agentctl"), cockpitBaseDir())
}

func TestCleanStaleStateDirsRemovesDeadPidDirs(t *testing.T) {
	base := t.TempDir()
	dead := cockpitStateDir(base, 999999) // a pid extremely unlikely to be alive
	require.NoError(t, os.MkdirAll(dead, 0o700))
	keep := cockpitStateDir(base, os.Getpid()) // our own pid: alive, must survive
	require.NoError(t, os.MkdirAll(keep, 0o700))

	cleanStaleStateDirs(base)

	_, err := os.Stat(dead)
	require.True(t, os.IsNotExist(err), "dead pid dir should be removed")
	_, err = os.Stat(keep)
	require.NoError(t, err, "live pid dir should survive")
}
