package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpawnCmdUsesGivenCwd(t *testing.T) {
	f := &fakeAPI{}
	msg := spawnCmd(f, "do the thing", "/work/api")()
	done, ok := msg.(spawnDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.NotNil(t, f.spawned)
	require.Equal(t, "/work/api", f.spawned.Cwd)
	require.Equal(t, "do the thing", f.spawned.Prompt)
}
