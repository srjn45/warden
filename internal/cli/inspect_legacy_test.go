package cli

import (
	"bytes"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func TestInspectLegacy_RefusesSchema1DataDir(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"inspect", "legacy", "--data-dir", dir})
	err := root.Execute()
	require.Error(t, err)
}

func TestInspectLegacy_HasNoWriteFlags(t *testing.T) {
	cmd := newInspectLegacyCmd()
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	require.ElementsMatch(t, []string{"json", "data-dir"}, names)
}
