package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateCleanupLegacy_RefusesNonSchema2(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"migrate", "--cleanup-legacy", "--yes", "--data-dir", t.TempDir()})
	require.Error(t, root.Execute())
}

func TestMigrateCleanupLegacy_ExclusiveWithOtherActions(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"migrate", "--cleanup-legacy", "--retirement", "--data-dir", t.TempDir()})
	require.ErrorContains(t, root.Execute(), "more than one")
}
