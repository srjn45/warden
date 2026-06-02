package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveDir(t *testing.T) {
	got, err := resolveDir("/explicit/path")
	require.NoError(t, err)
	require.Equal(t, "/explicit/path", got, "an explicit --dir wins")

	wd, err := os.Getwd()
	require.NoError(t, err)
	got, err = resolveDir("")
	require.NoError(t, err)
	require.Equal(t, wd, got, "empty --dir falls back to the current directory")
}
