package repl

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Arg-shaping eval: a labelled golden corpus that locks in the sanitizer's
// behaviour on the tool-call JSON the planner (Fast-Brain) emits.

type shapeCase struct {
	name string
	in   ToolCall
	want map[string]any
}

var goldenShape = []shapeCase{
	{"drops fabricated repo/model/type", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "review auth", "repo": "/path/to/repo", "model": "gpt-4", "type": "frobnicate"}},
		map[string]any{"prompt": "review auth"}},
	{"canonicalises valid enums", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "model": "Opus", "type": "PR-Review"}},
		map[string]any{"prompt": "x", "model": "opus", "type": "pr-review"}},
	{"keeps a real repo path", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "repo": "/home/me/dev/warden"}},
		map[string]any{"prompt": "x", "repo": "/home/me/dev/warden"}},
	{"drops empty strings, keeps non-strings", ToolCall{Name: "spawn_agent", Args: map[string]any{
		"prompt": "x", "name": "   ", "worktree": true}},
		map[string]any{"prompt": "x", "worktree": true}},
}

// TestEval_SanitizerGolden grades sanitizeCall against the shaping corpus so the
// hallucination-scrubbing behaviour stays pinned as the code evolves.
func TestEval_SanitizerGolden(t *testing.T) {
	for _, tc := range goldenShape {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sanitizeCall(tc.in).Args)
		})
	}
}
