package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeSelection(dir, "agent-4f98", 1700000000))
	require.Equal(t, "agent-4f98", readSelection(dir))
}

func TestSelectionMissingReturnsEmpty(t *testing.T) {
	require.Equal(t, "", readSelection(t.TempDir()))
	require.Equal(t, "", readSelection("")) // empty dir is a no-op
}

func TestSelectionCorruptReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "selection.json"), []byte("{not json"), 0o600))
	require.Equal(t, "", readSelection(dir))
}

func TestWriteSelectionEmptyDirIsNoop(t *testing.T) {
	require.NoError(t, writeSelection("", "x", 0)) // must not error or panic
}
