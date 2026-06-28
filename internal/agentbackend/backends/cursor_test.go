package backends

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// --- Command builders -------------------------------------------------------

func TestCursorLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "model + default mode (Cursor's own posture applies)",
			opts: agentbackend.LaunchOpts{Model: "composer-2.5", Mode: "default"},
			want: "cursor-agent --model 'composer-2.5'",
		},
		{
			name: "plan mode maps to --mode plan",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "plan"},
			want: "cursor-agent --model 'm' --mode plan",
		},
		{
			name: "ask mode maps to --mode ask",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "ask"},
			want: "cursor-agent --model 'm' --mode ask",
		},
		{
			name: "auto-review maps to --auto-review",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "auto-review"},
			want: "cursor-agent --model 'm' --auto-review",
		},
		{
			name: "force maps to -f (run everything)",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "force"},
			want: "cursor-agent --model 'm' -f",
		},
		{
			name: "claude 'dangerously-skip-permissions' folds onto -f",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "dangerously-skip-permissions"},
			want: "cursor-agent --model 'm' -f",
		},
		{
			name: "empty model omits --model (Cursor's configured default applies)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "cursor-agent",
		},
		{
			name: "session id and name are ignored (SessionIDControl=false)",
			opts: agentbackend.LaunchOpts{SessionID: "uuid", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "cursor-agent --model 'm'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Cursor{}.LaunchCmd(tt.opts))
		})
	}
}

// TestCursorLaunchNoOwnWorktree guards the double-worktree hazard: warden manages the
// git worktree, so the launch command must never contain Cursor's own -w/--worktree.
func TestCursorLaunchNoOwnWorktree(t *testing.T) {
	for _, mode := range []string{"default", "plan", "ask", "auto-review", "force"} {
		got := Cursor{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m", Mode: mode})
		require.NotContains(t, got, "-w", "warden owns the worktree; never pass Cursor's --worktree")
		require.NotContains(t, got, "--worktree")
	}
}

// TestCursorLaunchQuotesModel is the command-injection guard: a model string with
// shell metacharacters must be single-quoted so it cannot break out of the line typed
// into a tmux pane.
func TestCursorLaunchQuotesModel(t *testing.T) {
	got := Cursor{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m; touch /tmp/pwned #", Mode: "default"})
	require.Equal(t, "cursor-agent --model 'm; touch /tmp/pwned #'", got)
}

func TestCursorResumeCmd(t *testing.T) {
	// Cursor mints its own UUID chatId (same shape as warden's placeholder) so resume
	// is dir/workspace-scoped --continue, regardless of the passed SessionID.
	cmd, ok := Cursor{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "whatever", Model: "m"})
	require.True(t, ok, "Cursor supports resume (Caps.Resume=true)")
	require.Equal(t, "cursor-agent --continue --model 'm'", cmd)

	cmd, ok = Cursor{}.ResumeCmd(agentbackend.ResumeOpts{})
	require.True(t, ok)
	require.Equal(t, "cursor-agent --continue", cmd, "empty model omits --model")
}

func TestCursorLaunchPromptArg(t *testing.T) {
	got := Cursor{}.LaunchPromptArg("/state/prompts/job-1")
	require.Equal(t, ` "$(cat '/state/prompts/job-1')"`, got)
}

func TestCursorHeadlessCmd(t *testing.T) {
	argv, ok := Cursor{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"cursor-agent", "-p", "--force", "--trust", "classify this"}, argv)
}

// --- Transcript (Tier C: degraded) ------------------------------------------

// TestCursorTranscriptPathDegrades documents the Tier-C decision: the interactive
// transcript is an unreadable SQLite store.db, so TranscriptPath always degrades.
func TestCursorTranscriptPathDegrades(t *testing.T) {
	_, ok := Cursor{}.TranscriptPath("", "/work/agent-cursor", "uuid")
	require.False(t, ok, "no minimally-sourceable on-disk transcript (SQLite store.db)")
}

// TestCursorParseTranscript parses the real captured stream-json fixture (a file-edit
// session) into neutral Turns: the human prompt, the edit tool call (with the touched
// file), and the model's closing reply. The parser is not wired into the live digest
// path (StructuredTranscript=false) but is kept tested for the future Tier-A upgrade.
func TestCursorParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "cursor", "stream-toolcall.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Cursor{}.ParseTranscript(f)
	require.NoError(t, err)

	var users, assistants int
	for _, tr := range turns {
		switch tr.Role {
		case "user":
			users++
		case "assistant":
			assistants++
		}
	}
	require.Equal(t, 1, users, "system/result records are not turns")
	require.Equal(t, 2, assistants, "the edit tool call and the closing reply are both assistant turns")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "hi.txt")

	tool := turns[1]
	require.Equal(t, "assistant", tool.Role)
	require.Equal(t, "edit", tool.ToolName, "tool name derived from the editToolCall key")
	require.Len(t, tool.Files, 1)
	require.True(t, strings.HasSuffix(tool.Files[0], "hi.txt"), "touched file read from args.path")
	require.False(t, tool.Timestamp.IsZero(), "tool_call timestamp_ms applied")

	require.Contains(t, turns[2].Text, "hi.txt", "closing assistant reply captured")
}

// TestCursorParseTranscriptFold covers folding: a completed tool_call that follows an
// assistant text turn (no tool yet) folds onto it instead of starting a new turn, and
// a started/completed pair is counted once.
func TestCursorParseTranscriptFold(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"edit calc.py"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"editing now"}]}}`,
		`{"type":"tool_call","subtype":"started","tool_call":{"editToolCall":{"args":{"path":"calc.py"}},"toolCallId":"t1"}}`,
		`{"type":"tool_call","subtype":"completed","tool_call":{"editToolCall":{"args":{"path":"calc.py"}},"toolCallId":"t1"},"timestamp_ms":1782641064122}`,
	}, "\n")

	turns, err := Cursor{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 2, "the completed tool_call folds onto the assistant text turn; started is ignored")

	require.Equal(t, "user", turns[0].Role)
	a := turns[1]
	require.Equal(t, "assistant", a.Role)
	require.Equal(t, "editing now", a.Text)
	require.Equal(t, "edit", a.ToolName)
	require.Equal(t, []string{"calc.py"}, a.Files)
}

// TestCursorParseTranscriptTolerant skips malformed lines and ignores system/result
// records rather than erroring.
func TestCursorParseTranscriptTolerant(t *testing.T) {
	stream := strings.Join([]string{
		`not json at all`,
		`{"type":"system","subtype":"init","session_id":"x","model":"m"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"result","subtype":"success","result":"done","usage":{"inputTokens":1}}`,
		``,
	}, "\n")

	turns, err := Cursor{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 1)
	require.Equal(t, "hello", turns[0].Text)
}

// --- State / approval (degraded) --------------------------------------------

func TestCursorStateDegrades(t *testing.T) {
	require.Equal(t, agentbackend.StateUnknown, Cursor{}.DetectState("any pane content"))
	_, ok := Cursor{}.ParseApproval("Allow command? [y/n]")
	require.False(t, ok, "interactive approval parsing is deferred — degrade, not mis-parse")
}

// --- Capabilities / pricing -------------------------------------------------

func TestCursorCapabilities(t *testing.T) {
	c := Cursor{}.Capabilities()
	require.True(t, c.Resume, "Cursor resumes (cursor-agent --continue)")
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.False(t, c.StructuredTranscript, "Tier C: interactive transcript is an unreadable SQLite store")
	require.False(t, c.SessionIDControl, "Cursor mints its own UUID chatId")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"default", "plan", "ask", "auto-review", "force"}, c.PermissionModes)
}

func TestCursorNoPricing(t *testing.T) {
	_, ok := Cursor{}.Pricing()
	require.False(t, ok, "hosted plan ⇒ tokens surfaced but no warden-side dollar pricing")
}

func TestCursorSystemPromptUnsupported(t *testing.T) {
	_, ok := Cursor{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

func TestCursorRegistered(t *testing.T) {
	b, err := agentbackend.Get("cursor")
	require.NoError(t, err)
	require.Equal(t, "Cursor", b.DisplayName())
	require.Equal(t, "cursor-agent", b.Binary())
}
