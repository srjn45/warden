package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
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

func TestStartWithConfigDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// With model_default: haiku in the config file, spawning without an explicit
	// --model should use haiku.
	// Real test would verify Model field = "claude-haiku-4-5"
}

func TestStartWithHardcodedDefault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	// With model_default left at its default, spawning without an explicit
	// --model should use claude-sonnet-4-6.
	// Real test would verify Model field = "claude-sonnet-4-6"
}

func TestParseTags(t *testing.T) {
	require.Nil(t, parseTags(""), "empty flag yields no tags")
	require.Equal(t, []string{"backend", "urgent"}, parseTags("backend,urgent"))
	// Trims whitespace and drops blank segments; daemon does lowercase+dedup.
	require.Equal(t, []string{"Backend", "urgent"}, parseTags(" Backend , urgent , , "))
}

func TestStartTagsFlagRegistered(t *testing.T) {
	cmd := newStartCmd()
	f := cmd.Flags().Lookup("tags")
	require.NotNil(t, f, "--tags flag must be registered on start")
	require.Equal(t, "", f.DefValue, "--tags must default to empty")
}

// TestStartRequiresRole verifies --role is strictly mandatory: no implicit
// fallback to "general" for a spawn with no --role at all. This must fail
// before any daemon call, so it needs no running daemon to assert on.
func TestStartRequiresRole(t *testing.T) {
	cmd := newStartCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"do the thing"})
	err := cmd.Execute()
	require.Error(t, err, "spawning with no --role must fail")
	require.Contains(t, err.Error(), "--role is required")
}

// TestStartRequiresRoleBlankIsAlsoMissing verifies a whitespace-only --role is
// treated as missing, not as a (bogus) role name.
func TestStartRequiresRoleBlankIsAlsoMissing(t *testing.T) {
	cmd := newStartCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"do the thing", "--role", "   "})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "--role is required")
}

// TestStartUnknownRoleStillRejected keeps the existing unknown-role validation
// intact now that the empty-role case is handled separately.
func TestStartUnknownRoleStillRejected(t *testing.T) {
	cmd := newStartCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"do the thing", "--role", "not-a-real-role"})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown role")
}

// TestStartWithRolePassesRoleValidation confirms a valid --role clears the
// mandatory-role check (it may still fail later trying to reach a daemon —
// this only asserts the failure is not about role).
func TestStartWithRolePassesRoleValidation(t *testing.T) {
	cmd := newStartCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	// Set an invalid addr so it definitely fails on the daemon call, rather than
	// accidentally hitting a live user daemon and spawning a real "do the thing" agent!
	cmd.SetArgs([]string{"do the thing", "--role", "worker", "--addr", "127.0.0.1:1"})
	err := cmd.Execute()
	if err != nil {
		require.NotContains(t, err.Error(), "--role is required")
		require.NotContains(t, err.Error(), "unknown role")
	}
}

func TestStartAicliFlagRegistered(t *testing.T) {
	cmd := newStartCmd()
	require.NotNil(t, cmd.Flags().Lookup("aicli"), "--aicli must be the canonical flag")
	require.NotNil(t, cmd.Flags().Lookup("ai-cli"), "--ai-cli remains a backwards-compatible alias")
	require.NotNil(t, cmd.Flags().Lookup("backend"), "--backend remains a deprecated alias")
}

func TestResolveAiCliFlagPrecedence(t *testing.T) {
	cmd := newStartCmd()
	require.NoError(t, cmd.Flags().Set("aicli", "codex"))
	require.NoError(t, cmd.Flags().Set("ai-cli", "cursor"))
	require.NoError(t, cmd.Flags().Set("backend", "aider"))
	require.Equal(t, "codex", resolveAiCliFlag(cmd), "--aicli wins over aliases")

	cmd = newStartCmd()
	require.NoError(t, cmd.Flags().Set("ai-cli", "cursor"))
	require.NoError(t, cmd.Flags().Set("backend", "aider"))
	require.Equal(t, "cursor", resolveAiCliFlag(cmd), "--ai-cli wins over --backend")

	cmd = newStartCmd()
	require.NoError(t, cmd.Flags().Set("backend", "aider"))
	require.Equal(t, "aider", resolveAiCliFlag(cmd), "--backend alone still works")
}

// TestStartModelRequiresAicli verifies --model without --aicli fails before any
// daemon call, so a typo cannot silently spawn on the default AI CLI.
func TestStartModelRequiresAicli(t *testing.T) {
	cmd := newStartCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"do the thing", "--role", "worker", "--model", "opus"})
	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "--model requires --aicli")
}

// TestStartModelWithAicliAliasPassesValidation confirms --ai-cli satisfies the
// --model pairing check (it may still fail later on the daemon call).
func TestStartModelWithAicliAliasPassesValidation(t *testing.T) {
	cmd := newStartCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"do the thing", "--role", "worker", "--ai-cli", "claude", "--model", "opus", "--addr", "127.0.0.1:1"})
	err := cmd.Execute()
	if err != nil {
		require.NotContains(t, err.Error(), "--model requires --aicli")
	}
}

func TestStartSwitchHiddenFlagsStillParse(t *testing.T) {
	hidden := map[string]*cobra.Command{
		"start":  newStartCmd(),
		"switch": newSwitchCmd(),
	}
	want := map[string][]string{
		"start":  {"ai-cli", "backend", "supervised", "type"},
		"switch": {"ai-cli", "backend"},
	}
	for name, cmd := range hidden {
		for _, f := range want[name] {
			fl := cmd.Flags().Lookup(f)
			require.NotNil(t, fl, "%s --%s must exist", name, f)
			require.True(t, fl.Hidden, "%s --%s must be hidden", name, f)
		}
		require.False(t, cmd.Flags().Lookup("aicli").Hidden, "%s --aicli stays visible", name)
	}
	start := hidden["start"]
	require.NoError(t, start.Flags().Parse([]string{"--ai-cli", "codex", "--supervised", "--type", "development", "--backend", "aider"}))
	require.Equal(t, "codex", startFlag(t, start, "ai-cli"))
	require.Equal(t, "aider", startFlag(t, start, "backend"))
}

func startFlag(t *testing.T, c *cobra.Command, name string) string {
	t.Helper()
	v, err := c.Flags().GetString(name)
	require.NoError(t, err)
	return v
}
