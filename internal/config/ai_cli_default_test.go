package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAiCliDefaultCanonical(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("ai_cli_default: aider\n"), 0o600))
	c := Load(path)
	require.Equal(t, "aider", c.GetAiCliDefault())
}

func TestBackendDefaultDeprecatedAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("backend_default: codex\n"), 0o600))
	c := Load(path)
	require.Equal(t, "codex", c.GetAiCliDefault(), "deprecated backend_default must populate ai_cli_default")
}

func TestAiCliDefaultWinsOverBackendDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("ai_cli_default: aider\nbackend_default: claude\n"), 0o600))
	c := Load(path)
	require.Equal(t, "aider", c.GetAiCliDefault(), "canonical ai_cli_default must win when both are set")
}
