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

func TestAntigravityLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "model + default mode (agy's own posture applies)",
			opts: agentbackend.LaunchOpts{Model: "Gemini 3.5 Flash (Low)", Mode: "default"},
			want: "agy --model 'Gemini 3.5 Flash (Low)'",
		},
		{
			name: "sandbox mode maps to --sandbox",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "sandbox"},
			want: "agy --model 'm' --sandbox",
		},
		{
			name: "dangerously-skip-permissions passes through",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "dangerously-skip-permissions"},
			want: "agy --model 'm' --dangerously-skip-permissions",
		},
		{
			name: "claude 'bypassPermissions' folds onto --dangerously-skip-permissions",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "bypassPermissions"},
			want: "agy --model 'm' --dangerously-skip-permissions",
		},
		{
			name: "empty model omits --model (agy config default applies)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "agy",
		},
		{
			name: "session id and name are ignored (SessionIDControl=false)",
			opts: agentbackend.LaunchOpts{SessionID: "uuid", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "agy --model 'm'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Antigravity{}.LaunchCmd(tt.opts))
		})
	}
}

// TestAntigravityLaunchQuotesModel is the command-injection guard: a model string
// with shell metacharacters must be single-quoted so it cannot break out of the line
// typed into a tmux pane.
func TestAntigravityLaunchQuotesModel(t *testing.T) {
	got := Antigravity{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m; touch /tmp/pwned #", Mode: "default"})
	require.Equal(t, "agy --model 'm; touch /tmp/pwned #'", got)
}

func TestAntigravityResumeCmd(t *testing.T) {
	// agy mints its own UUID id warden cannot pin (and warden's placeholder is also a
	// UUID), so resume is dir-scoped `agy -c`.
	cmd, ok := Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "whatever", Model: "m"})
	require.True(t, ok, "Antigravity supports resume (Caps.Resume=true)")
	require.Equal(t, "agy -c --model 'm'", cmd)

	// Permission mode flows through resume too.
	cmd, _ = Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{Mode: "dangerously-skip-permissions"})
	require.Equal(t, "agy -c --dangerously-skip-permissions", cmd)
}

func TestAntigravityLaunchPromptArg(t *testing.T) {
	// Prompt is seeded via -i (--prompt-interactive): run it, then stay interactive.
	got := Antigravity{}.LaunchPromptArg("/state/prompts/job-1")
	require.Equal(t, ` -i "$(cat '/state/prompts/job-1')"`, got)
}

func TestAntigravityHeadlessCmd(t *testing.T) {
	argv, ok := Antigravity{}.HeadlessCmd("classify this")
	require.True(t, ok)
	// Prompt must immediately follow -p (it is the flag's value).
	require.Equal(t, []string{"agy", "--dangerously-skip-permissions", "-p", "classify this"}, argv)
}

// --- Transcript resolution (dir-scoped) -------------------------------------

// TestAntigravityTranscriptPathDirScoped points agyHome at the fixture tree and
// resolves the trajectory log by mapping the workdir to its conv-id via
// cache/last_conversations.json.
func TestAntigravityTranscriptPathDirScoped(t *testing.T) {
	withAgyHome(t, filepath.Join("testdata", "antigravity"))

	p, ok := Antigravity{}.TranscriptPath("", "/work/agent-agy", "warden-placeholder-uuid")
	require.True(t, ok, "transcript for the workdir resolves")
	require.True(t, strings.HasSuffix(p, "transcript.jsonl"))
	require.Contains(t, p, filepath.Join("brain", "81c29863-bf4f-41ba-bf92-e5865ab529f1"))
}

func TestAntigravityTranscriptPathDegrades(t *testing.T) {
	withAgyHome(t, filepath.Join("testdata", "antigravity"))

	// No workdir ⇒ nothing to resolve.
	_, ok := Antigravity{}.TranscriptPath("", "", "")
	require.False(t, ok)

	// A directory with no entry in last_conversations.json ⇒ degrade.
	_, ok = Antigravity{}.TranscriptPath("", "/work/no-such-agent", "")
	require.False(t, ok)

	// An agyHome with no store at all ⇒ degrade, no error.
	withAgyHome(t, t.TempDir())
	_, ok = Antigravity{}.TranscriptPath("", "/work/agent-agy", "")
	require.False(t, ok)
}

// withAgyHome temporarily points the agyHome resolver at dir for the test.
func withAgyHome(t *testing.T, dir string) {
	t.Helper()
	prev := agyHome
	agyHome = func() string { return dir }
	t.Cleanup(func() { agyHome = prev })
}

// --- Transcript parsing -----------------------------------------------------

// TestAntigravityParseTranscript parses the real captured trajectory fixture (a
// two-turn `agy -p` + `agy -c -p` conversation) and asserts the neutral Turns
// warden's digest depends on: the human prompts and the model replies, with `agy`'s
// SYSTEM control records (CONVERSATION_HISTORY / CHECKPOINT / SYSTEM_MESSAGE) dropped.
func TestAntigravityParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "antigravity", "brain",
		"81c29863-bf4f-41ba-bf92-e5865ab529f1", ".system_generated", "logs", "transcript.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Antigravity{}.ParseTranscript(f)
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
	require.Equal(t, 2, users, "SYSTEM control records are dropped; both human prompts surface")
	require.Equal(t, 2, assistants)

	require.Equal(t, "user", turns[0].Role)
	// The <USER_REQUEST> body is unwrapped; the appended metadata/settings blocks are gone.
	require.Equal(t, "Reply with exactly this token and nothing else: WARDEN_FIXTURE_OK", turns[0].Text)
	require.NotContains(t, turns[0].Text, "ADDITIONAL_METADATA")
	require.False(t, turns[0].Timestamp.IsZero(), "created_at timestamp applied")

	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "WARDEN_FIXTURE_OK", turns[1].Text)
}

// TestAntigravityParseTranscriptTolerant skips malformed lines and ignores SYSTEM /
// unknown records rather than erroring.
func TestAntigravityParseTranscriptTolerant(t *testing.T) {
	stream := strings.Join([]string{
		`not json at all`,
		`{"step_index":0,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE"}`,
		`{"step_index":1,"source":"SYSTEM","type":"CHECKPOINT","status":"DONE","content":"summary"}`,
		`{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-06-28T10:00:00Z","content":"plain prompt without wrapper"}`,
		`{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-06-28T10:00:01Z","content":"reply"}`,
		``,
	}, "\n")

	turns, err := Antigravity{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "user", turns[0].Role)
	require.Equal(t, "plain prompt without wrapper", turns[0].Text, "content with no <USER_REQUEST> wrapper is used as-is")
	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "reply", turns[1].Text)
}

// --- State / approval (live markers) ----------------------------------------

// agyFixture reads a captured tmux-pane fixture from testdata/antigravity/.
func agyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "antigravity", name))
	require.NoError(t, err)
	return string(b)
}

// TestAntigravityDetectState classifies each captured pane: an at-rest pane ⇒ Idle
// (the "? for shortcuts" footer), a streaming turn ⇒ Working (the "esc to cancel"
// footer / "Generating..." spinner), and an open permission prompt ⇒ NeedsInput.
func TestAntigravityDetectState(t *testing.T) {
	tests := []struct {
		fixture string
		want    agentbackend.State
	}{
		{"state-idle.txt", agentbackend.StateIdle},
		{"state-working.txt", agentbackend.StateWorking},
		{"approval.txt", agentbackend.StateNeedsInput},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			require.Equal(t, tt.want, Antigravity{}.DetectState(agyFixture(t, tt.fixture)))
		})
	}

	// An unrecognized pane stays Unknown (no false positive ⇒ warden infers idle
	// from staleness).
	require.Equal(t, agentbackend.StateUnknown, Antigravity{}.DetectState("just some quiet output"))
}

// TestAntigravityParseApproval parses the captured shell-command permission prompt
// into the neutral Approval: the proposed command (Action), the "Do you want to
// proceed?" header (Question), the four options top-down (1-indexed), the highlighted
// option (SelectedIdx), and the least-privilege non-sticky "Yes" (AffirmativeIdx).
func TestAntigravityParseApproval(t *testing.T) {
	a, ok := Antigravity{}.ParseApproval(agyFixture(t, "approval.txt"))
	require.True(t, ok, "the captured permission prompt parses")

	require.Equal(t, "echo hello-from-agy", a.Action)
	require.Equal(t, "Do you want to proceed?", a.Question)
	require.Equal(t, []string{
		"Yes",
		"Yes, and always allow in this conversation for commands that start with 'echo'",
		"Yes, and always allow for commands that start with 'echo' (Persist to settings.json)",
		"No",
	}, a.Options)
	require.Equal(t, 1, a.SelectedIdx, "the > cursor sits on option 1")
	require.Equal(t, 1, a.AffirmativeIdx, "least-privilege affirmative is the bare non-sticky Yes")
	require.False(t, a.AffirmativeSticky, "option 1 is a one-shot grant, not a standing one")
}

// TestAntigravityParseApprovalNegative proves a non-approval pane (idle or working)
// is NOT mis-read as an approval — the header gate keeps the auto-approve path honest.
func TestAntigravityParseApprovalNegative(t *testing.T) {
	for _, name := range []string{"state-idle.txt", "state-working.txt"} {
		t.Run(name, func(t *testing.T) {
			_, ok := Antigravity{}.ParseApproval(agyFixture(t, name))
			require.False(t, ok)
		})
	}

	// A bare numbered list in agent prose (no "Do you want to proceed?" header) is
	// not an approval, even though it has sequential 1..N lines.
	prose := "Here are the steps:\n  1. Yes do this\n  2. No skip that\n"
	_, ok := Antigravity{}.ParseApproval(prose)
	require.False(t, ok, "a numbered list without the permission header is not a prompt")
}

// TestAntigravityAffirmativeStickyFallback covers the case where the only affirmative
// is a standing "always allow" grant: it is chosen with sticky=true.
func TestAntigravityAffirmativeStickyFallback(t *testing.T) {
	idx, sticky := agyAffirmative([]string{
		"Yes, and always allow for commands that start with 'ls'",
		"No",
	})
	require.Equal(t, 1, idx)
	require.True(t, sticky)

	idx, sticky = agyAffirmative([]string{"No", "No, and tell agy what to do"})
	require.Equal(t, 0, idx, "no affirmative offered")
	require.False(t, sticky)
}

// --- Capabilities / pricing -------------------------------------------------

func TestAntigravityCapabilities(t *testing.T) {
	c := Antigravity{}.Capabilities()
	require.True(t, c.Resume, "Antigravity resumes (agy -c)")
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.True(t, c.StructuredTranscript, "Tier A: trajectory JSONL parses into Turns")
	require.False(t, c.SessionIDControl, "agy mints its own UUID conversation id")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"default", "sandbox", "dangerously-skip-permissions"}, c.PermissionModes)
}

func TestAntigravityNoPricing(t *testing.T) {
	_, ok := Antigravity{}.Pricing()
	require.False(t, ok, "Google-hosted free tier ⇒ no warden-side dollar pricing table yet")
}

func TestAntigravitySystemPromptUnsupported(t *testing.T) {
	_, ok := Antigravity{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

// TestAntigravityIdentity covers the canonical backend id and binary name.
func TestAntigravityIdentity(t *testing.T) {
	a := Antigravity{}
	require.Equal(t, "antigravity", a.ID(), "canonical backend id is 'antigravity'")
	require.Equal(t, "agy", a.Binary(), "binary is agy")
	require.Equal(t, "Antigravity", a.DisplayName())
}
