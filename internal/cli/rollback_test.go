package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRollbackCmdHelp(t *testing.T) {
	cmd := newRootCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"rollback", "--help"})

	err := cmd.Execute()
	require.NoError(t, err)

	out := buf.String()
	require.Contains(t, out, "Roll back the most recent warden update")
	require.Contains(t, out, "--yes")
	require.Contains(t, out, "--ready-timeout")
	require.Contains(t, out, "--json")
}
