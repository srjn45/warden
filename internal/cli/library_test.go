package cli

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/preset"
	"github.com/stretchr/testify/require"
)

// runLibrary executes a library subcommand with --config pointed at a config
// path (presets.yaml is derived from its directory), returning combined output.
func runLibrary(t *testing.T, configPath string, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"library"}, append(args, "--config", configPath)...))
	require.NoError(t, root.Execute())
	return buf.String()
}

// TestLibraryListShowsBothSections renders presets + templates in two sections.
func TestLibraryListShowsBothSections(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	// Seed a preset so the presets section is non-empty.
	store := &preset.Store{}
	store.Set("review", preset.Preset{Type: "pr-review", Model: "opus", PermissionMode: "acceptEdits"})
	require.NoError(t, store.Save(filepath.Join(filepath.Dir(cfg), "presets.yaml")))

	out := runLibrary(t, cfg, "list")

	// Presets section, with the seeded preset and its defaults.
	require.Contains(t, out, "SPAWN PRESETS")
	require.Contains(t, out, "review")
	require.Contains(t, out, "type=pr-review")
	require.Contains(t, out, "model=opus")

	// Templates section, with a known built-in template.
	require.Contains(t, out, "PIPELINE TEMPLATES")
	require.Contains(t, out, "analyze-implement-review")
}

// TestLibraryListEmptyPresets still shows both labeled sections.
func TestLibraryListEmptyPresets(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	out := runLibrary(t, cfg, "list")
	require.Contains(t, out, "SPAWN PRESETS")
	require.Contains(t, out, "none saved")
	require.Contains(t, out, "PIPELINE TEMPLATES")
}

// TestLibrarySavePresetDelegates writes through the same preset store as
// `warden preset save`.
func TestLibrarySavePresetDelegates(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")

	out := runLibrary(t, cfg, "save-preset", "code-review", "--type", "pr-review", "--model", "opus", "--supervised")
	require.Contains(t, out, "saved preset")
	require.Contains(t, out, "code-review")

	store, err := preset.Load(filepath.Join(filepath.Dir(cfg), "presets.yaml"))
	require.NoError(t, err)
	p, ok := store.Get("code-review")
	require.True(t, ok)
	require.Equal(t, "pr-review", p.Type)
	require.Equal(t, "opus", p.Model)
	require.Equal(t, "acceptEdits", p.PermissionMode)

	// The saved preset is then visible via `library list`.
	list := runLibrary(t, cfg, "list")
	require.Contains(t, list, "code-review")
}
