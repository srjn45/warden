package backends

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// TestFindMenuOnCapturedPanes pins the wording-free menu gate (the cost gate in
// front of model-assisted prompt recognition) against every captured pane: it
// must see the real cursor menus and must NOT fire on an idle, working,
// rate-limited or picker pane — a false positive there would cost a model call
// for every agent that merely finished a turn.
func TestFindMenuOnCapturedPanes(t *testing.T) {
	menus := map[string]bool{
		"antigravity/approval.txt":             true,
		"antigravity/approval-run-command.txt": true, // lands with the agy 1.2.17 fixture
		"antigravity/trust-prompt.txt":         true,
		"claude/trust-prompt.txt":              true,
		"codex/approval-command.txt":           true,
		"codex/trust-prompt.txt":               true,
		"cursor/approval.txt":                  true,
	}
	files, err := filepath.Glob(filepath.Join("testdata", "*", "*.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		rel, _ := filepath.Rel("testdata", f)
		rel = filepath.ToSlash(rel)
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		_, ok := agentbackend.FindMenu(string(b))
		require.Equal(t, menus[rel], ok, rel)
	}
}
