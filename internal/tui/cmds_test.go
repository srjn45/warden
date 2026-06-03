package tui

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// spawnCmd must send a cwd: prompt-mode spawns require one (the daemon rejects
// an empty cwd with 400). Agents launch in the directory the TUI runs in.
func TestSpawnCmdSendsCwd(t *testing.T) {
	f := &fakeAPI{}
	_ = spawnCmd(f, "do a thing")() // execute the tea.Cmd
	require.NotNil(t, f.spawned, "spawn was invoked")
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, wd, f.spawned.Cwd, "spawn includes the process working directory")
	require.NotEmpty(t, f.spawned.Cwd)
}
