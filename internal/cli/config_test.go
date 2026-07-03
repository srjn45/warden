package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runConfig executes the config command (or a subcommand) with --config pointed
// at path, returning combined stdout.
func runConfig(t *testing.T, path string, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"config", "--config", path}, args...))
	require.NoError(t, root.Execute())
	return buf.String()
}

func TestConfigInitThenShow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	initOut := runConfig(t, path, "init")
	require.Contains(t, initOut, "config ready")
	require.FileExists(t, path)

	out := runConfig(t, path)
	require.Contains(t, out, path)
	require.Contains(t, out, "default_permission_mode")
	require.Contains(t, out, "auto")
	require.Contains(t, out, "[daemon]")
	require.Contains(t, out, "[rate limit (rate_limit.*)]")
}

func TestConfigShowMissingFileNotesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	out := runConfig(t, path)
	require.Contains(t, out, "file not found")
	require.Contains(t, out, "claude-sonnet-4-6") // default model still shown
}

func TestConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	out := runConfig(t, path, "path")
	require.Equal(t, path, strings.TrimSpace(out))
}

func TestConfigInitIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	runConfig(t, path, "init")
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	runConfig(t, path, "init")
	second, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}
