package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateCommandFlags(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"update"})
	require.NoError(t, err)
	require.Equal(t, "update", cmd.Name())
	require.NotNil(t, cmd.Flags().Lookup("check"))
	require.NotNil(t, cmd.Flags().Lookup("force"))
	require.NotNil(t, cmd.Flags().Lookup("version"))
}

func TestUpdateCommandHelpMentionsAtomicSwap(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"update", "--help"})
	require.NoError(t, root.Execute())
	require.Contains(t, out.String(), "atomically")
	require.Contains(t, out.String(), "--check")
}
