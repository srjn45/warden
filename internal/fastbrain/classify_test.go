package fastbrain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentbackend/backends"
)

func fixture(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "agentbackend", "backends", "testdata"}, parts...)...)
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return string(b)
}

func TestClassifyPromptBackends(t *testing.T) {
	claudePane := strings.Join([]string{
		" Bash command", "", "   rm -f /tmp/probe", "",
		" Do you want to proceed?",
		" ❯ 1. Yes", "   2. Yes, and always allow access", "   3. No", "", " Esc to cancel",
	}, "\n")

	tests := []struct {
		name    string
		backend agentbackend.Backend
		pane    string
		want    PromptCategory
	}{
		{"claude yes/no", backends.Claude{}, claudePane, CategoryToolPermission},
		{"codex command", backends.Codex{}, fixture(t, "codex", "approval-command.txt"), CategoryToolPermission},
		{"cursor command", backends.Cursor{}, fixture(t, "cursor", "approval.txt"), CategoryToolPermission},
		{"cursor trust", backends.Cursor{}, fixture(t, "cursor", "trust-prompt.txt"), CategoryStrategicQuestion},
		{"antigravity command", backends.Antigravity{}, fixture(t, "antigravity", "approval.txt"), CategoryToolPermission},
		{"antigravity trust", backends.Antigravity{}, fixture(t, "antigravity", "trust-prompt.txt"), CategoryStrategicQuestion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, ok := tc.backend.ParseApproval(tc.pane)
			require.True(t, ok)
			require.Equal(t, tc.want, ClassifyPrompt(a))
		})
	}
}

func TestClassifyPromptSynthetic(t *testing.T) {
	tests := []struct {
		name string
		a    *agentbackend.Approval
		want PromptCategory
	}{
		{"nil", nil, CategoryStrategicQuestion},
		{"empty", &agentbackend.Approval{}, CategoryStrategicQuestion},
		{"tool shape", &agentbackend.Approval{Action: "Bash(ls)", Options: []string{"Yes", "No"}, AffirmativeIdx: 1}, CategoryToolPermission},
		{"which option no affirmative", &agentbackend.Approval{Question: "Which file?", Options: []string{"a.go", "b.go"}}, CategoryStrategicQuestion},
		{"non-yes affirmative", &agentbackend.Approval{Question: "Which?", Options: []string{"Use Postgres", "Use SQLite"}, AffirmativeIdx: 1}, CategoryStrategicQuestion},
		{"allow/deny", &agentbackend.Approval{Question: "Allow?", Options: []string{"Allow", "Deny"}, AffirmativeIdx: 1}, CategoryToolPermission},
		{"affirmative out of range", &agentbackend.Approval{Options: []string{"Yes"}, AffirmativeIdx: 3}, CategoryStrategicQuestion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ClassifyPrompt(tc.a))
		})
	}
}
