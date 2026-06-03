package tui

import (
	"testing"

	"github.com/srajanpathak/agentctl/internal/approval"
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

func TestApprovalsCmdEmitsMsg(t *testing.T) {
	fa := &fakeAPI{approvalsOn: true, approvals: []approval.View{{ID: "a1", Recognized: true, Options: []string{"Yes", "No"}, Fingerprint: "ff"}}}
	msg := approvalsCmd(fa)()
	am, ok := msg.(approvalsMsg)
	require.True(t, ok)
	require.True(t, am.enabled)
	require.Len(t, am.views, 1)
}
