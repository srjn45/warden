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

func TestStartWithModelFlag(t *testing.T) {
	// This test requires running daemon - mark as integration test
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Test spawning with explicit model
	// Note: This is a minimal example - actual test may need daemon setup
	cmd := newStartCmd()
	cmd.SetArgs([]string{"test task", "--model", "opus"})

	// Would need to capture output and verify agent was spawned with opus model
	// This is a placeholder for actual integration test
	// Real test would:
	// 1. Start daemon
	// 2. Run spawn with --model opus
	// 3. Query session and verify Model field = "claude-opus-4-8"
	// 4. Clean up
}

func TestStartWithEnvVarDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Set env var
	t.Setenv("WARDEN_MODEL_DEFAULT", "haiku")

	// Test spawning without explicit model
	// Should use haiku from env var
	// Real test would verify Model field = "claude-haiku-4-5"
}

func TestStartWithHardcodedDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// Ensure env var not set
	t.Setenv("WARDEN_MODEL_DEFAULT", "")

	// Test spawning without explicit model
	// Should use claude-sonnet-4-5 default
	// Real test would verify Model field = "claude-sonnet-4-5"
}
