package backends

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// --- Command builders -------------------------------------------------------

func TestAiderLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "model + prompt mode (default)",
			opts: agentbackend.LaunchOpts{Model: "ollama_chat/qwen2.5-coder:3b", Mode: "default"},
			want: "aider --no-show-model-warnings --model 'ollama_chat/qwen2.5-coder:3b'",
		},
		{
			name: "auto mode maps to --yes-always",
			opts: agentbackend.LaunchOpts{Model: "gpt-4o", Mode: "yes-always"},
			want: "aider --no-show-model-warnings --model 'gpt-4o' --yes-always",
		},
		{
			name: "claude 'auto' folds onto --yes-always",
			opts: agentbackend.LaunchOpts{Model: "gpt-4o", Mode: "auto"},
			want: "aider --no-show-model-warnings --model 'gpt-4o' --yes-always",
		},
		{
			name: "empty model omits --model (BYO model)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "aider --no-show-model-warnings",
		},
		{
			name: "session id and name are ignored",
			opts: agentbackend.LaunchOpts{SessionID: "abc-123", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "aider --no-show-model-warnings --model 'm'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Aider{}.LaunchCmd(tt.opts))
		})
	}
}

// TestAiderLaunchQuotesModel is the command-injection guard: a model string with
// shell metacharacters must be single-quoted so it cannot break out of the line
// typed into a tmux pane.
func TestAiderLaunchQuotesModel(t *testing.T) {
	got := Aider{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m; touch /tmp/pwned #", Mode: "default"})
	require.Equal(t, "aider --no-show-model-warnings --model 'm; touch /tmp/pwned #'", got)
}

func TestAiderResumeUnsupported(t *testing.T) {
	_, ok := Aider{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "x"})
	require.False(t, ok, "Aider has no resume-by-id (Caps.Resume=false)")
}

func TestAiderLaunchPromptArg(t *testing.T) {
	got := Aider{}.LaunchPromptArg("/state/prompts/job-1")
	require.Equal(t, ` --message "$(cat '/state/prompts/job-1')"`, got)
}

func TestAiderHeadlessCmd(t *testing.T) {
	argv, ok := Aider{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"aider", "--no-show-model-warnings", "--yes-always", "--no-auto-commits", "--message", "classify this"}, argv)
}

// --- Transcript -------------------------------------------------------------

func TestAiderTranscriptPath(t *testing.T) {
	dir := t.TempDir()

	_, ok := Aider{}.TranscriptPath("", dir, "")
	require.False(t, ok, "no transcript file yet ⇒ ok=false")

	p := filepath.Join(dir, aiderTranscriptName)
	require.NoError(t, os.WriteFile(p, []byte("# aider chat started at 2026-06-27 19:25:05\n"), 0o644))

	got, ok := Aider{}.TranscriptPath("ignored-projects-dir", dir, "ignored-session-id")
	require.True(t, ok)
	require.Equal(t, p, got)
}

func TestAiderTranscriptPathEmptyWorkdir(t *testing.T) {
	_, ok := Aider{}.TranscriptPath("p", "", "s")
	require.False(t, ok)
}

// TestAiderParseTranscript parses the real captured multi-turn fixture and
// asserts the neutral Turns warden's digest depends on.
func TestAiderParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "aider", "chat-history.md"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Aider{}.ParseTranscript(f)
	require.NoError(t, err)

	// Two sessions, each one user prompt + one assistant response.
	var users, assistants int
	var edited []string
	for _, tr := range turns {
		switch tr.Role {
		case "user":
			users++
		case "assistant":
			assistants++
			edited = append(edited, tr.Files...)
		}
	}
	require.Equal(t, 2, users, "two #### user prompts")
	require.Equal(t, 2, assistants, "two assistant responses")
	require.Equal(t, []string{"calc.py", "math2.py"}, edited, "edited files from 'Applied edit to' lines")

	// First turn is the first user prompt (the digest "Task").
	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "implement add to return a+b")

	// The assistant turn carries the edited code as its body and an edit tool.
	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "edit", turns[1].ToolName)
	require.Contains(t, turns[1].Text, "def add(a, b):")
	require.Contains(t, turns[1].Text, "def subtract(a, b):")

	// Session timestamp is parsed onto the turns.
	require.False(t, turns[0].Timestamp.IsZero(), "session header timestamp applied")
}

// --- State / approval -------------------------------------------------------

func TestAiderDetectState(t *testing.T) {
	approvalPane, err := os.ReadFile(filepath.Join("testdata", "aider", "approval-gitignore.txt"))
	require.NoError(t, err)
	require.Equal(t, agentbackend.StateNeedsInput, Aider{}.DetectState(string(approvalPane)))

	idlePane, err := os.ReadFile(filepath.Join("testdata", "aider", "pane-idle.txt"))
	require.NoError(t, err)
	require.Equal(t, agentbackend.StateUnknown, Aider{}.DetectState(string(idlePane)),
		"no open prompt ⇒ Unknown (idle inferred from staleness)")
}

func TestAiderParseApprovalYesNo(t *testing.T) {
	pane, err := os.ReadFile(filepath.Join("testdata", "aider", "approval-gitignore.txt"))
	require.NoError(t, err)

	a, ok := Aider{}.ParseApproval(string(pane))
	require.True(t, ok)
	require.Equal(t, []string{"Yes", "No"}, a.Options)
	require.Equal(t, 1, a.AffirmativeIdx, "Yes is the affirmative")
	require.Equal(t, 1, a.SelectedIdx, "[Yes] is the default")
	require.Contains(t, a.Question, "Add .aider* to .gitignore")
}

func TestAiderParseApprovalMultiOption(t *testing.T) {
	pane, err := os.ReadFile(filepath.Join("testdata", "aider", "approval-addfile.txt"))
	require.NoError(t, err)

	a, ok := Aider{}.ParseApproval(string(pane))
	require.True(t, ok)
	require.Equal(t, []string{"Yes", "No", "All", "Skip all"}, a.Options)
	require.Equal(t, 1, a.AffirmativeIdx)
	require.Equal(t, 1, a.SelectedIdx)
}

func TestAiderParseApprovalNone(t *testing.T) {
	_, ok := Aider{}.ParseApproval("just some output\nno prompt here\n")
	require.False(t, ok)
}

// --- Capabilities / pricing -------------------------------------------------

func TestAiderCapabilities(t *testing.T) {
	c := Aider{}.Capabilities()
	require.False(t, c.Resume)
	require.True(t, c.Headless)
	require.True(t, c.StructuredTranscript, "Tier A: markdown transcript is parseable")
	require.False(t, c.SessionIDControl)
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"default", "yes-always"}, c.PermissionModes)
}

func TestAiderNoPricing(t *testing.T) {
	_, ok := Aider{}.Pricing()
	require.False(t, ok, "BYO model ⇒ no pricing table (spend degrades to tokens-only)")
}

func TestAiderRegistered(t *testing.T) {
	b, err := agentbackend.Get("aider")
	require.NoError(t, err)
	require.Equal(t, "Aider", b.DisplayName())
}
