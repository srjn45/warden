package repl

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeCall_DropsHallucinatedSpawnArgs(t *testing.T) {
	in := ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt":          "review the auth package",
		"repo":            "/path/to/repo", // fabricated placeholder
		"model":           "gpt-4",         // not a warden model
		"type":            "frobnicate",    // not a real type
		"permission_mode": "yolo",          // out of range
		"name":            "  ",            // empty after trim
	}}
	got := sanitizeCall(in)
	require.Equal(t, map[string]any{"prompt": "review the auth package"}, got.Args,
		"only the real, in-range fields survive")
}

func TestSanitizeCall_KeepsAndCanonicalisesValidEnums(t *testing.T) {
	in := ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt":          "do a thing",
		"model":           "Opus",      // valid but mis-cased
		"type":            "PR-Review", // valid but mis-cased
		"permission_mode": "plan",
	}}
	got := sanitizeCall(in)
	require.Equal(t, "opus", got.Args["model"], "enum value is canonicalised, not dropped")
	require.Equal(t, "pr-review", got.Args["type"])
	require.Equal(t, "plan", got.Args["permission_mode"])
	require.Equal(t, "do a thing", got.Args["prompt"])
}

func TestSanitizeCall_KeepsRealRepoPath(t *testing.T) {
	in := ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "repo": "/home/me/dev/warden",
	}}
	got := sanitizeCall(in)
	require.Equal(t, "/home/me/dev/warden", got.Args["repo"], "a real path the operator gave is untouched")
}

func TestSanitizeCall_DropsPlaceholderDirVariants(t *testing.T) {
	for _, val := range []string{"<repo>", "/path/to/thing", ".../foo", "your-repo/x"} {
		got := sanitizeCall(ToolCall{Name: "commit", Args: map[string]any{"agent": "a1", "dir": val}})
		_, present := got.Args["dir"]
		require.False(t, present, "placeholder dir %q should be dropped", val)
		require.Equal(t, "a1", got.Args["agent"], "the real field survives")
	}
}

func TestSanitizeCall_LeavesNonStringArgsAlone(t *testing.T) {
	in := ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "worktree": true, "lines": float64(50),
	}}
	got := sanitizeCall(in)
	require.Equal(t, true, got.Args["worktree"], "a bool isn't touched")
	require.Equal(t, float64(50), got.Args["lines"], "a number isn't touched")
}

func TestSanitizeCall_NoArgsIsUntouched(t *testing.T) {
	in := ToolCall{Name: "list_agents"}
	got := sanitizeCall(in)
	require.Nil(t, got.Args)
}
