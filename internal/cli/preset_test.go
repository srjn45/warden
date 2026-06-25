package cli

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/preset"
	"github.com/stretchr/testify/require"
)

// runPreset executes a preset subcommand with --config pointed at a config path
// (presets.yaml is derived from its directory), returning combined output.
func runPreset(t *testing.T, configPath string, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"preset"}, append(args, "--config", configPath)...))
	require.NoError(t, root.Execute())
	return buf.String()
}

func TestPresetSaveThenList(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")

	out := runPreset(t, cfg, "save", "code-review", "--type", "pr-review", "--model", "opus", "--supervised")
	require.Contains(t, out, "saved preset")
	require.Contains(t, out, "code-review")

	// The store landed beside the config file and recorded the flags, with
	// --supervised normalized to permission_mode=acceptEdits.
	store, err := preset.Load(filepath.Join(filepath.Dir(cfg), "presets.yaml"))
	require.NoError(t, err)
	p, ok := store.Get("code-review")
	require.True(t, ok)
	require.Equal(t, "pr-review", p.Type)
	require.Equal(t, "opus", p.Model)
	require.Equal(t, "acceptEdits", p.PermissionMode)

	list := runPreset(t, cfg, "list")
	require.Contains(t, list, "code-review")
	require.Contains(t, list, "type=pr-review")
	require.Contains(t, list, "model=opus")
	require.Contains(t, list, "permission-mode=acceptEdits")
}

func TestPresetSaveOverwrites(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	runPreset(t, cfg, "save", "p1", "--type", "spike")
	out := runPreset(t, cfg, "save", "p1", "--type", "analysis", "--worktree")
	require.Contains(t, out, "updated preset")

	store, err := preset.Load(filepath.Join(filepath.Dir(cfg), "presets.yaml"))
	require.NoError(t, err)
	p, _ := store.Get("p1")
	require.Equal(t, "analysis", p.Type)
	require.True(t, p.Worktree)
}

func TestPresetListEmpty(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	out := runPreset(t, cfg, "list")
	require.Contains(t, out, "no presets saved")
}

func TestStartPresetResolutionAndOverride(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	// Seed a preset with several defaults.
	store := &preset.Store{}
	store.Set("review", preset.Preset{Type: "pr-review", Model: "opus", PermissionMode: "acceptEdits", AutoRestart: true})
	require.NoError(t, store.Save(filepath.Join(filepath.Dir(cfg), "presets.yaml")))

	cmd := newStartCmd()
	cmd.Flags().String("config", "", "") // stand in for the root persistent flag
	require.NoError(t, cmd.ParseFlags([]string{"--config", cfg, "--preset", "review", "--model", "sonnet"}))

	pre, err := loadStartPreset(cmd)
	require.NoError(t, err)
	require.Equal(t, "pr-review", pre.Type)

	// Unset flags inherit the preset; the explicit --model overrides it.
	require.Equal(t, "pr-review", stringFlagOr(cmd, "type", pre.Type))
	require.Equal(t, "acceptEdits", stringFlagOr(cmd, "permission-mode", pre.PermissionMode))
	require.True(t, boolFlagOr(cmd, "auto-restart", pre.AutoRestart))
	require.Equal(t, "sonnet", stringFlagOr(cmd, "model", pre.Model)) // CLI flag wins
}

func TestLoadStartPresetMissingErrors(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	cmd := newStartCmd()
	cmd.Flags().String("config", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"--config", cfg, "--preset", "ghost"}))
	_, err := loadStartPreset(cmd)
	require.ErrorContains(t, err, "ghost")
}

func TestLoadStartPresetEmptyIsZero(t *testing.T) {
	cmd := newStartCmd()
	cmd.Flags().String("config", "", "")
	require.NoError(t, cmd.ParseFlags(nil))
	pre, err := loadStartPreset(cmd)
	require.NoError(t, err)
	require.Equal(t, preset.Preset{}, pre)
}
