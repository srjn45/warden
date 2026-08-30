package backends

import (
	"errors"
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

// TestCursorLaunchPromptArgEmpty pins that Cursor puts nothing on the launch line:
// the trailing positional only populates cursor-agent's composer without submitting
// it, so the prompt is typed in and submitted after launch via PromptSeeder instead.
func TestCursorLaunchPromptArgEmpty(t *testing.T) {
	require.Equal(t, "", Cursor{}.LaunchPromptArg("/state/prompts/job-1"))
}

// TestCursorPromptSeeder locks the PromptSeeder seam: warden types the task into
// cursor-agent's composer once ready and presses Enter (so cursor-agent auto-submits
// it, which the launch-line positional does not). An empty prompt disables seeding,
// and ReadyMarker keys on cursor's fresh-launch composer placeholder.
func TestCursorPromptSeeder(t *testing.T) {
	var ps agentbackend.PromptSeeder = Cursor{}
	text, ok := ps.PromptText("do the thing")
	require.True(t, ok)
	require.Equal(t, "do the thing", text)

	_, ok = ps.PromptText("")
	require.False(t, ok, "an empty prompt disables post-launch seeding")

	require.Equal(t, "Plan, search, build anything", ps.ReadyMarker())
	require.Contains(t, cursorIdlePlaceholders, ps.ReadyMarker(),
		"the ready marker is one of cursor's live composer placeholders")
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

// --- State / approval (live markers) ----------------------------------------

// cursorFixture reads a captured tmux-pane fixture from testdata/cursor/.
func cursorFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cursor", name))
	require.NoError(t, err)
	return string(b)
}

// TestCursorDetectState classifies each captured pane: a streaming turn ⇒ Working
// (the "ctrl+c to stop" hint), an open command approval or the workspace-trust
// prompt ⇒ NeedsInput, an at-rest composer ⇒ Idle (fresh and post-turn placeholders).
func TestCursorDetectState(t *testing.T) {
	tests := []struct {
		fixture string
		want    agentbackend.State
	}{
		{"state-working.txt", agentbackend.StateWorking},
		{"approval.txt", agentbackend.StateNeedsInput},
		{"trust-prompt.txt", agentbackend.StateNeedsInput},
		{"state-idle.txt", agentbackend.StateIdle},
		{"state-idle-after-turn.txt", agentbackend.StateIdle},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			require.Equal(t, tt.want, Cursor{}.DetectState(cursorFixture(t, tt.fixture)))
		})
	}

	// An unrecognized pane stays Unknown (no false positive ⇒ warden uses staleness).
	require.Equal(t, agentbackend.StateUnknown, Cursor{}.DetectState("just some quiet output"))
}

// TestCursorParseApprovalCommand parses the captured command-allowlist approval into
// the neutral Approval: the proposed command (Action, with cursor's " in <cwd>" hint
// stripped), the "Run this command?" header (Question), the four options top-down
// (1-indexed, key hints kept), the highlighted option (SelectedIdx), and the
// least-privilege non-sticky "Run (once)" (AffirmativeIdx).
func TestCursorParseApprovalCommand(t *testing.T) {
	a, ok := Cursor{}.ParseApproval(cursorFixture(t, "approval.txt"))
	require.True(t, ok, "the captured command approval parses")

	require.Equal(t, "echo hello-from-cursor", a.Action)
	require.Equal(t, "Run this command?", a.Question)
	require.Equal(t, []string{
		"Run (once) (y)",
		"Add Shell(echo) to allowlist? (tab)",
		"Run Everything (shift+tab)",
		"Skip (esc or n)",
	}, a.Options)
	require.Equal(t, 1, a.SelectedIdx, "the → cursor sits on option 1")
	require.Equal(t, 1, a.AffirmativeIdx, "least-privilege affirmative is the one-shot Run (once)")
	require.False(t, a.AffirmativeSticky, "Run (once) is a one-shot grant, not a standing one")
}

// TestCursorParseApprovalTrust parses the captured workspace-trust prompt: warden
// surfaces it as an Approval (so the operator can clear it from the inbox). The
// affirmative "Trust this workspace" is a standing grant (sticky).
func TestCursorParseApprovalTrust(t *testing.T) {
	a, ok := Cursor{}.ParseApproval(cursorFixture(t, "trust-prompt.txt"))
	require.True(t, ok, "the captured workspace-trust prompt parses")

	require.Equal(t, "Do you trust the contents of this directory?", a.Question)
	require.Equal(t, []string{"Trust this workspace", "Quit"}, a.Options)
	require.Equal(t, 1, a.SelectedIdx, "the ▶ cursor sits on Trust this workspace")
	require.Equal(t, 1, a.AffirmativeIdx)
	require.True(t, a.AffirmativeSticky, "trusting a workspace persists to .workspace-trusted")
	require.Contains(t, a.Action, "cursortrust", "the trusted directory is surfaced as the Action")
}

// TestCursorParseApprovalNegative proves a non-approval pane (idle or working) is NOT
// mis-read as an approval — the question/banner gates keep the auto-approve path
// honest.
func TestCursorParseApprovalNegative(t *testing.T) {
	for _, name := range []string{"state-idle.txt", "state-idle-after-turn.txt", "state-working.txt"} {
		t.Run(name, func(t *testing.T) {
			_, ok := Cursor{}.ParseApproval(cursorFixture(t, name))
			require.False(t, ok)
		})
	}

	// A single line carrying trailing parens (cursor's "Reason for rejection (…)"
	// composer prompt) is not a menu: it needs ≥2 contiguous options and a "?" header.
	_, ok := Cursor{}.ParseApproval("  → Reason for rejection (Enter to submit, Esc to cancel)")
	require.False(t, ok, "a lone parenthesized composer prompt is not an approval menu")
}

// TestCursorAffirmativeStickyFallback covers the case where the only affirmative is a
// standing "allowlist" grant: it is chosen with sticky=true; a menu with only a
// decline returns no affirmative.
func TestCursorAffirmativeStickyFallback(t *testing.T) {
	idx, sticky := cursorAffirmative([]string{
		"Add Shell(rm) to allowlist? (tab)",
		"Skip (esc or n)",
	})
	require.Equal(t, 1, idx)
	require.True(t, sticky)

	idx, sticky = cursorAffirmative([]string{"Skip (esc or n)", "No (n)"})
	require.Equal(t, 0, idx, "no affirmative offered")
	require.False(t, sticky)
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

// --- Model menu (live `cursor-agent --list-models`) -------------------------

// TestCursorImplementsModelLister locks the ModelLister seam for cursor: unlike Claude
// (static alias table), Cursor exposes a live hosted menu, so it MUST implement
// agentbackend.ModelLister — that is what lights up `wd models` for cursor with no
// change to the generic verb.
func TestCursorImplementsModelLister(t *testing.T) {
	_, ok := agentbackend.Backend(Cursor{}).(agentbackend.ModelLister)
	require.True(t, ok, "Cursor has a live model menu; wd models must offer it")
}

// TestParseCursorModels locks the parser against the REAL `cursor-agent --list-models`
// output captured live (testdata/cursor/models.txt, cursor-agent 2026.06.26-7079533):
// an "Available models" header, blank-separated "<id> - <Display Name>" lines, and a
// trailing "Tip: …" footer. Only the ids (left of " - ") are kept, in order; the
// header and footer (no " - ") drop out. The ids are the exact `--model` labels.
func TestParseCursorModels(t *testing.T) {
	out := cursorFixture(t, "models.txt")
	got := parseCursorModels([]byte(out))

	require.NotEmpty(t, got)
	require.Equal(t, "auto", got[0], "first listed id is 'auto'")
	require.Equal(t, "glm-5.2-max", got[len(got)-1], "last listed id")

	// A representative spread of the live ids must round-trip verbatim (these feed
	// --model directly).
	for _, id := range []string{"composer-2.5-fast", "claude-opus-4-8-thinking-high", "gpt-5.5-high", "gemini-3.5-flash"} {
		require.Contains(t, got, id)
	}

	// The header and the Tip footer must NOT leak into the menu.
	require.NotContains(t, got, "Available models")
	for _, m := range got {
		require.NotContains(t, m, "Tip:", "the trailing tip line must not be parsed as a model")
		require.NotContains(t, m, " ", "ids carry no spaces (the display name is dropped)")
	}
}

// TestParseCursorModelsTrimsAndDropsBlanks covers the edge shapes the parser handles:
// surrounding whitespace, blank lines, lines without the " - " separator, and the
// id/name split. Never nil so JSON emits [].
func TestParseCursorModelsTrimsAndDropsBlanks(t *testing.T) {
	in := "Available models\n\n  foo - Foo Model  \n\nbar-baz - Bar Baz\njust a sentence with no separator\n"
	require.Equal(t, []string{"foo", "bar-baz"}, parseCursorModels([]byte(in)))
	require.Equal(t, []string{}, parseCursorModels(nil), "empty input ⇒ empty (non-nil) slice")
}

// TestCursorListModels drives ListModels through the stubbed `cursor-agent
// --list-models` runner (the fixture stands in for the live binary) and asserts it
// normalizes the menu to ids.
func TestCursorListModels(t *testing.T) {
	orig := cursorListModelsCmd
	t.Cleanup(func() { cursorListModelsCmd = orig })
	cursorListModelsCmd = func() ([]byte, error) {
		return []byte(cursorFixture(t, "models.txt")), nil
	}
	models, ok := Cursor{}.ListModels()
	require.True(t, ok)
	require.NotEmpty(t, models)
	require.Equal(t, "auto", models[0])
	require.Contains(t, models, "composer-2.5-fast")
}

// TestCursorListModelsDegradesOnError: a command error (binary missing / not signed
// in) returns ok=false so `wd models` reports a clean degrade, never a crash.
func TestCursorListModelsDegradesOnError(t *testing.T) {
	orig := cursorListModelsCmd
	t.Cleanup(func() { cursorListModelsCmd = orig })
	cursorListModelsCmd = func() ([]byte, error) {
		return nil, errors.New("exec: cursor-agent: not found")
	}
	_, ok := Cursor{}.ListModels()
	require.False(t, ok, "a command error degrades to ok=false")
}
