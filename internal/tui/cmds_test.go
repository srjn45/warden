package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpawnCmdUsesGivenCwd(t *testing.T) {
	f := &fakeAPI{}
	msg := spawnCmd(f, "do the thing", "my-agent", "/work/api", "reviewer", "terminal", false)()
	done, ok := msg.(spawnDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.NotNil(t, f.spawned)
	require.Equal(t, "/work/api", f.spawned.Cwd)
	require.Equal(t, "do the thing", f.spawned.Prompt)
	require.Equal(t, "my-agent", f.spawned.Name)
	require.Equal(t, "reviewer", f.spawned.Role)
	require.Equal(t, "terminal", f.spawned.Backend)
}
