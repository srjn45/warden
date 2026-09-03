package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFactoryResetRequiresYes(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"factory-reset", "--scope", "data"})
	require.NoError(t, root.Execute())
	require.Contains(t, out.String(), "Re-run with --yes")
}

func TestFactoryResetInvalidScope(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"factory-reset", "--scope", "nope", "--yes"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid --scope")
}
