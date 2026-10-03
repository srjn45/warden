package tui

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetupTUILogging_CustomPath(t *testing.T) {
	tmpDir := t.TempDir()
	logFile := filepath.Join(tmpDir, "custom-tui.log")
	t.Setenv("WARDEN_TUI_LOG", logFile)

	cleanup := setupTUILogging()
	slog.Info("test message for tui log", "foo", "bar")
	cleanup()

	content, err := os.ReadFile(logFile)
	require.NoError(t, err)
	require.Contains(t, string(content), "test message for tui log")
	require.Contains(t, string(content), "foo=bar")
}

func TestSetupTUILogging_Off(t *testing.T) {
	for _, val := range []string{"off", "discard", "/dev/null"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("WARDEN_TUI_LOG", val)
			cleanup := setupTUILogging()
			slog.Info("should be discarded", "k", "v")
			cleanup()
		})
	}
}

func TestSetupTUILogging_DefaultPath(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("WARDEN_TUI_LOG", "")

	cleanup := setupTUILogging()
	slog.Warn("reattach warning", "attempt", 2)
	cleanup()

	defaultLog := filepath.Join(tmpHome, ".warden", "tui.log")
	content, err := os.ReadFile(defaultLog)
	require.NoError(t, err)
	require.Contains(t, string(content), "reattach warning")
	require.Contains(t, string(content), "attempt=2")
}

func TestSetupTUILogging_CleanupRestores(t *testing.T) {
	orig := slog.Default()
	t.Setenv("WARDEN_TUI_LOG", "discard")

	cleanup := setupTUILogging()
	require.NotSame(t, orig, slog.Default())

	cleanup()
	require.Same(t, orig, slog.Default())
}
