package cli

import (
	"os"
	"path/filepath"
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

	// A relative --dir is resolved to absolute against the caller's cwd.
	rel, err := resolveDir("subdir")
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(rel), "relative --dir must be absolutized, got %q", rel)
	expected, err := filepath.Abs("subdir")
	require.NoError(t, err)
	require.Equal(t, expected, rel)
}

func TestCurrentTmuxSessionNotInTmux(t *testing.T) {
	t.Setenv("TMUX", "") // not inside tmux
	require.Equal(t, "", currentTmuxSession())
}

func TestStartSupervisedFlagRegistered(t *testing.T) {
	cmd := newStartCmd()
	f := cmd.Flags().Lookup("supervised")
	require.NotNil(t, f, "--supervised flag must be registered on start")
	require.Equal(t, "false", f.DefValue, "--supervised must default to false")
}

func TestPromptFromArgs(t *testing.T) {
	require.Equal(t, "fix the bug", promptFromArgs([]string{"fix the bug"}), "single arg is the prompt")
	require.Equal(t, "", promptFromArgs(nil), "no args means an interactive (empty-prompt) spawn")
	require.Equal(t, "", promptFromArgs([]string{}), "no args means an interactive (empty-prompt) spawn")
}
