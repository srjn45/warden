package backends

import (
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

func TestTranslateModeTable(t *testing.T) {
	cases := []struct{ from, mode, to, want string }{
		{"antigravity", "dangerously-skip-permissions", "claude", "bypassPermissions"},
		{"claude", "bypassPermissions", "antigravity", "dangerously-skip-permissions"},
		{"claude", "bypassPermissions", "codex", "danger-full-access"},
		{"codex", "danger-full-access", "cursor", "force"},
		{"cursor", "force", "aider", "yes-always"},
		{"aider", "yes-always", "crush", "yolo"},
		{"crush", "yolo", "opencode", "dangerously-skip-permissions"},
		{"opencode", "dangerously-skip-permissions", "goose", "auto"},
		{"claude", "acceptEdits", "codex", "workspace-write"},
		{"codex", "workspace-write", "antigravity", "accept-edits"},
		{"antigravity", "accept-edits", "claude", "acceptEdits"},
		{"claude", "plan", "codex", "read-only"},
		{"codex", "read-only", "claude", "plan"},
		{"cursor", "ask", "claude", "plan"},
		{"claude", "default", "aider", "default"},
	}
	for _, c := range cases {
		got, ok := agentbackend.TranslateMode(mustGet(t, c.from), mustGet(t, c.to), c.mode)
		require.True(t, ok, "%s %s → %s", c.from, c.mode, c.to)
		require.Equal(t, c.want, got, "%s %s → %s", c.from, c.mode, c.to)
	}
}

func TestTranslateModeNoEquivalent(t *testing.T) {
	for _, c := range []struct{ from, mode, to string }{
		{"claude", "dontAsk", "codex"},
		{"claude", "auto", "antigravity"},
		{"claude", "default", "codex"},
		{"antigravity", "sandbox", "claude"},
		{"claude", "acceptEdits", "aider"},
	} {
		_, ok := agentbackend.TranslateMode(mustGet(t, c.from), mustGet(t, c.to), c.mode)
		require.False(t, ok, "%s %s → %s", c.from, c.mode, c.to)
	}
}

// Every translation across every backend pair yields a mode the target declares.
func TestTranslateModeAlwaysAcceptedByTarget(t *testing.T) {
	for _, f := range agentbackend.IDs() {
		for _, to := range agentbackend.IDs() {
			from, tb := mustGet(t, f), mustGet(t, to)
			for _, m := range from.Capabilities().PermissionModes {
				got, ok := agentbackend.TranslateMode(from, tb, m)
				if ok {
					require.True(t, agentbackend.ModeAccepted(tb, got), "%s %q → %s gave %q", f, m, to, got)
				}
			}
		}
	}
}

func mustGet(t *testing.T, id string) agentbackend.Backend {
	t.Helper()
	b, err := agentbackend.Get(id)
	require.NoError(t, err)
	return b
}
