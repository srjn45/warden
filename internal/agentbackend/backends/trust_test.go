package backends

import (
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// TestTrustPromptParse pins how each backend's captured workspace-trust prompt
// normalizes: it must be marked as a trust prompt, name the directory, and point
// at the "yes, trust" option with the cursor position the answer path needs.
func TestTrustPromptParse(t *testing.T) {
	cases := []struct {
		backend     string
		options     []string
		selected    int
		affirmative int
		navigate    bool
	}{
		// Claude highlights "No, exit" by default: a bare Enter would quit it.
		{"claude", []string{"No, exit", "Yes, I trust this folder"}, 1, 2, true},
		{"codex", []string{"Trust and continue", "Quit"}, 1, 1, true},
		{"antigravity", []string{"Yes, I trust this folder", "No, exit"}, 1, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.backend, func(t *testing.T) {
			b, err := agentbackend.Get(tc.backend)
			require.NoError(t, err)
			a, ok := b.ParseApproval(loadPaneFixture(t, tc.backend, "trust-prompt.txt"))
			require.True(t, ok)
			require.Equal(t, agentbackend.ApprovalKindTrust, a.Kind)
			require.Equal(t, tc.options, a.Options)
			require.Equal(t, tc.selected, a.SelectedIdx)
			require.Equal(t, tc.affirmative, a.AffirmativeIdx)
			require.Equal(t, tc.navigate, a.Navigate)
			require.True(t, a.AffirmativeSticky)
			require.NotEmpty(t, a.Action, "the directory under question is the Action")
		})
	}
}

// TestTrustPromptNotMisparsed guards the recognition gates: ordinary prompts and
// prose that mentions trust must not be classified as a trust prompt.
func TestTrustPromptNotMisparsed(t *testing.T) {
	a, ok := Codex{}.ParseApproval(loadPaneFixture(t, "codex", "approval-command.txt"))
	require.True(t, ok)
	require.Empty(t, a.Kind)
	require.False(t, a.Navigate)

	_, ok = Codex{}.ParseApproval("Trust this folder? Sure, here are the steps:\n  1. open it\n  2. read it\n")
	require.False(t, ok, "a numbered list under trust prose is not the prompt (no Folder access title)")

	_, ok = claudeParseTrustApproval("I'll check the folder you trust.\nEnter to confirm\n")
	require.False(t, ok)
}
