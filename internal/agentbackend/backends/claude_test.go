package backends

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/stretchr/testify/require"
)

// --- Command builders (byte-identical exit gate) ----------------------------

func TestPermissionFlag(t *testing.T) {
	tests := []struct{ mode, want string }{
		{"auto", "--permission-mode 'auto'"},
		{"acceptEdits", "--permission-mode 'acceptEdits'"},
		{"bypassPermissions", "--permission-mode 'bypassPermissions'"},
		{"default", "--permission-mode 'default'"},
		{"dontAsk", "--permission-mode 'dontAsk'"},
		{"plan", "--permission-mode 'plan'"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			if got := permissionFlag(tt.mode); got != tt.want {
				t.Errorf("permissionFlag(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestBase(t *testing.T) {
	tests := []struct{ mode, want string }{
		{"auto", "claude --model 'claude-sonnet-4-6' --permission-mode 'auto'"},
		{"acceptEdits", "claude --model 'claude-sonnet-4-6' --permission-mode 'acceptEdits'"},
		{"bypassPermissions", "claude --model 'claude-sonnet-4-6' --permission-mode 'bypassPermissions'"},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			if got := base("claude-sonnet-4-6", tt.mode); got != tt.want {
				t.Errorf("base(%q) = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

// TestBaseQuotesModel is the command-injection regression guard: a model string
// with shell metacharacters must be single-quoted so it can never break out of
// the launch line typed into a tmux pane.
func TestBaseQuotesModel(t *testing.T) {
	got := base("sonnet; touch /tmp/pwned #", "auto")
	want := `claude --model 'sonnet; touch /tmp/pwned #' --permission-mode 'auto'`
	if got != want {
		t.Errorf("base with injected model = %q, want %q", got, want)
	}
}

func TestLaunchCmd(t *testing.T) {
	got := Claude{}.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: "sid", Name: "agent-1", Model: "claude-sonnet-4-6", Mode: "auto",
	})
	want := "claude --model 'claude-sonnet-4-6' --permission-mode 'auto' --session-id 'sid' --name 'agent-1'"
	require.Equal(t, want, got)
	require.NotContains(t, got, "--dangerously-skip-permissions")
}

// TestLaunchCmdQuotesSessionID guards the fix for the import/adopt shell-injection
// finding: a stored ClaudeSessionID with shell metacharacters must be neutralized
// by single-quoting, not interpolated raw into the pane launch line.
func TestLaunchCmdQuotesSessionID(t *testing.T) {
	got := Claude{}.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: "x; touch /tmp/pwned #", Name: "agent-1", Model: "m", Mode: "auto",
	})
	require.Contains(t, got, `--session-id 'x; touch /tmp/pwned #'`)
	require.NotContains(t, got, "--session-id x;")
}

func TestLaunchCmdQuotesName(t *testing.T) {
	// A ticket-style name with a quote must be single-quoted safely.
	got := Claude{}.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: "sid", Name: "it's-a-ticket", Model: "m", Mode: "auto",
	})
	require.Contains(t, got, `--name 'it'\''s-a-ticket'`)
}

func TestResumeCmd(t *testing.T) {
	got, ok := Claude{}.ResumeCmd(agentbackend.ResumeOpts{
		SessionID: "sid", Name: "agent-1", Model: "claude-sonnet-4-6", Mode: "acceptEdits",
	})
	require.True(t, ok, "Claude supports resume")
	require.Equal(t, "claude --model 'claude-sonnet-4-6' --permission-mode 'acceptEdits' --resume 'sid' --name 'agent-1'", got)
}

// TestResumeCmdQuotesSessionID guards the fix for the import/adopt shell-injection
// finding on the resume path (Restore/Adopt carry a stored ClaudeSessionID).
func TestResumeCmdQuotesSessionID(t *testing.T) {
	got, ok := Claude{}.ResumeCmd(agentbackend.ResumeOpts{
		SessionID: "x; touch /tmp/pwned #", Name: "agent-1", Model: "m", Mode: "auto",
	})
	require.True(t, ok)
	require.Contains(t, got, `--resume 'x; touch /tmp/pwned #'`)
	require.NotContains(t, got, "--resume x;")
}

func TestHeadlessCmd(t *testing.T) {
	argv, ok := Claude{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"claude", "-p", "classify this"}, argv)
}

// --- Transcript path --------------------------------------------------------

func TestClaudeProjectDir(t *testing.T) {
	got := claudeProjectDir("/root/projects", "/Users/srajan.pathak/warden-agents/agent-a1b2")
	require.Equal(t, "/root/projects/-Users-srajan-pathak-warden-agents-agent-a1b2", got)
	require.Equal(t, "", claudeProjectDir("", "/anything")) // empty root → no lookup
}

func TestTranscriptPathBySessionIDBeatsNewest(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/warden-agents/agent-zz99"
	dir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	sid := "33333333-3333-4333-8333-333333333333"
	want := filepath.Join(dir, sid+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("HELLO"), 0o644))
	decoy := filepath.Join(dir, "decoy.jsonl")
	require.NoError(t, os.WriteFile(decoy, []byte("DECOY"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(decoy, future, future))

	got, ok := Claude{}.TranscriptPath(root, workdir, sid)
	require.True(t, ok)
	require.Equal(t, want, got, "pinned id beats newest-mtime decoy")
}

func TestTranscriptPathGlobFallback(t *testing.T) {
	root := t.TempDir()
	sid := "44444444-4444-4444-8444-444444444444"
	other := filepath.Join(root, "-some-other-encoded-dir")
	require.NoError(t, os.MkdirAll(other, 0o755))
	want := filepath.Join(other, sid+".jsonl")
	require.NoError(t, os.WriteFile(want, []byte("X"), 0o644))

	got, ok := Claude{}.TranscriptPath(root, "/mismatch/dir", sid)
	require.True(t, ok)
	require.Equal(t, want, got, "unique glob finds it despite dir mismatch")
}

func TestTranscriptPathLegacyFallsBackToNewest(t *testing.T) {
	root := t.TempDir()
	workdir := "/Users/me/warden-agents/agent-leg"
	dir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old.jsonl"), []byte("OLD"), 0o644))
	newf := filepath.Join(dir, "new.jsonl")
	require.NoError(t, os.WriteFile(newf, []byte("NEW"), 0o644))
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(newf, future, future))

	got, ok := Claude{}.TranscriptPath(root, workdir, "") // no pinned id
	require.True(t, ok)
	require.Equal(t, newf, got, "empty id -> newest .jsonl (legacy)")
}

func TestTranscriptPathUnresolved(t *testing.T) {
	_, ok := Claude{}.TranscriptPath("", "/x", "sid")
	require.False(t, ok, "empty projectsDir → not found")
}

// --- Transcript parse -------------------------------------------------------

func TestParseTranscript(t *testing.T) {
	jsonl := strings.Join([]string{
		`{"type":"user","timestamp":"2026-06-27T10:00:00Z","message":{"role":"user","content":"fix the auth bug"}}`,
		`{"type":"assistant","timestamp":"2026-06-27T10:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"On it."},{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/auth.go"}}]}}`,
		`not-json-skip-me`,
		`{"type":"summary"}`,
	}, "\n")

	turns, err := Claude{}.ParseTranscript(strings.NewReader(jsonl))
	require.NoError(t, err)
	require.Len(t, turns, 2)

	require.Equal(t, "user", turns[0].Role)
	require.Equal(t, "fix the auth bug", turns[0].Text)

	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "On it.", turns[1].Text)
	require.Equal(t, "Edit", turns[1].ToolName)
	require.Equal(t, []string{"/repo/auth.go"}, turns[1].Files)
	require.False(t, turns[1].Timestamp.IsZero(), "timestamp parsed")
}

// --- State / approval -------------------------------------------------------

func TestDetectState(t *testing.T) {
	require.Equal(t, agentbackend.StateWorking, Claude{}.DetectState("… (esc to interrupt)"))
	require.Equal(t, agentbackend.StateNeedsInput, Claude{}.DetectState("Do you want to proceed?\n ❯ 1. Yes"))
	require.Equal(t, agentbackend.StateNeedsInput, Claude{}.DetectState("❯ 1. Yes"))
	require.Equal(t, agentbackend.StateUnknown, Claude{}.DetectState("just some quiet output"))
}

func TestParseApprovalDelegates(t *testing.T) {
	pane := strings.Join([]string{
		"────────────────────────────────",
		" Bash command",
		"",
		"   rm -f /tmp/probe",
		"",
		" Do you want to proceed?",
		" ❯ 1. Yes",
		"   2. Yes, and always allow access",
		"   3. No",
		"",
		" Esc to cancel",
	}, "\n")

	want, wantOK := approval.Parse(pane)
	got, gotOK := Claude{}.ParseApproval(pane)
	require.Equal(t, wantOK, gotOK)
	require.True(t, gotOK, "fixture should be a recognized approval prompt")
	require.Equal(t, want.Action, got.Action)
	require.Equal(t, want.Question, got.Question)
	require.Equal(t, want.Options, got.Options)
	require.Equal(t, want.SelectedIdx, got.SelectedIdx)
	require.Equal(t, want.AffirmativeIdx, got.AffirmativeIdx)
	require.Equal(t, want.AffirmativeSticky, got.AffirmativeSticky)
}

func TestParseApprovalNoPrompt(t *testing.T) {
	got, ok := Claude{}.ParseApproval("just normal output, no prompt here")
	require.False(t, ok)
	require.Nil(t, got)
}

// --- System prompt / pricing / caps -----------------------------------------

func TestSystemPromptFlag(t *testing.T) {
	frag, ok := Claude{}.SystemPromptFlag("be nice")
	require.True(t, ok)
	require.Equal(t, " --append-system-prompt 'be nice'", frag)
	require.True(t, strings.HasPrefix(frag, " "), "leading space so it concatenates onto LaunchCmd")
}

// TestClaudeNotContextInjector regression-locks the InjectContext seam: Claude
// delivers its addendum via the launch-time --append-system-prompt flag and must NOT
// implement agentbackend.ContextInjector, so the AGENTS.md injection path never runs
// for it and its launch command stays byte-identical.
func TestClaudeNotContextInjector(t *testing.T) {
	_, ok := agentbackend.Backend(Claude{}).(agentbackend.ContextInjector)
	require.False(t, ok, "Claude uses the system-prompt flag, not AGENTS.md injection")
}

// TestClaudeNotReviewer regression-locks the Reviewer seam: Claude has no
// non-interactive native diff-review subcommand (its `/code-review` is a Claude-Code
// skill, not a warden-drivable verb), so it must NOT implement agentbackend.Reviewer
// — `wd review` is simply not offered for Claude (it degrades to `wd check` /
// `pr-review`), and Claude's launch/resume paths stay untouched.
func TestClaudeNotReviewer(t *testing.T) {
	_, ok := agentbackend.Backend(Claude{}).(agentbackend.Reviewer)
	require.False(t, ok, "Claude has no native review subcommand; wd review must skip it")
}

func TestPricing(t *testing.T) {
	tbl, ok := Claude{}.Pricing()
	require.True(t, ok)
	require.Contains(t, tbl.Models, "sonnet")
	require.Greater(t, tbl.Models["sonnet"].InputPerMTok, 0.0)
	require.Greater(t, tbl.Default.InputPerMTok, 0.0)
}

func TestCapabilitiesAllTrue(t *testing.T) {
	c := Claude{}.Capabilities()
	require.True(t, c.Resume)
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.True(t, c.StructuredTranscript)
	require.True(t, c.SystemPromptInject)
	require.True(t, c.SessionIDControl)
	require.NotEmpty(t, c.PermissionModes)
}

func TestIdentity(t *testing.T) {
	c := Claude{}
	require.Equal(t, "claude", c.ID())
	require.Equal(t, "claude", c.Binary())
	require.Equal(t, "Claude Code", c.DisplayName())
	require.NotEmpty(t, c.InstallHint())
}

// TestRegistered confirms the init() registration makes Claude the resolvable
// default — the wiring lifecycle relies on.
func TestRegistered(t *testing.T) {
	b, err := agentbackend.Get("claude")
	require.NoError(t, err)
	require.Equal(t, "claude", b.ID())
	require.NotNil(t, agentbackend.Default())
	require.Equal(t, "claude", agentbackend.Default().ID())
}
