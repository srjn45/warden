package backends

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestAntigravityParseTranscriptToolCalls parses the real captured tool-using
// trajectory fixture (one interactive `agy` v1.0.16 session that created a file and
// ran a shell command) and asserts the tool-call / files-changed extraction the
// digest's "what changed" column depends on: each `tool_calls` record surfaces as an
// assistant Turn carrying the tool name, and the file-bearing `write_to_file` call
// carries the JSON-decoded TargetFile path in Files.
func TestAntigravityParseTranscriptToolCalls(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "antigravity", "brain",
		"7f05dc62-68b3-40a1-82b9-115546ed9592", ".system_generated", "logs", "transcript.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Antigravity{}.ParseTranscript(f)
	require.NoError(t, err)
	require.Len(t, turns, 4, "user prompt, two tool steps, final prose reply")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "Create a new file named hello.txt")

	// The write_to_file call: tool name + the JSON-decoded TargetFile (no stray quotes).
	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "write_to_file", turns[1].ToolName)
	require.Equal(t, []string{"/home/srjn45/.gemini/antigravity-cli/scratch/hello.txt"}, turns[1].Files)
	require.False(t, turns[1].Timestamp.IsZero(), "created_at timestamp applied to the tool turn")

	// The run_command call: tool name, no file args ⇒ no Files.
	require.Equal(t, "assistant", turns[2].Role)
	require.Equal(t, "run_command", turns[2].ToolName)
	require.Empty(t, turns[2].Files)

	// The closing prose PLANNER_RESPONSE stays a plain assistant text turn.
	require.Equal(t, "assistant", turns[3].Role)
	require.Contains(t, turns[3].Text, "I have successfully")
	require.Empty(t, turns[3].ToolName)
}

// TestAntigravityToolCallFoldsOntoProse covers a PLANNER_RESPONSE carrying BOTH prose
// content and a tool call: the call folds onto the just-emitted text turn (the
// Codex/Cursor fold shape) instead of duplicating a Turn.
func TestAntigravityToolCallFoldsOntoProse(t *testing.T) {
	stream := `{"step_index":0,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-07-05T09:00:00Z","content":"Editing now.","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"\"/w/a.go\""}}]}` + "\n"

	turns, err := Antigravity{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 1)
	require.Equal(t, "Editing now.", turns[0].Text)
	require.Equal(t, "write_to_file", turns[0].ToolName)
	require.Equal(t, []string{"/w/a.go"}, turns[0].Files)
}

// TestAgyArgString locks the args-value decoding: `agy` JSON-encodes every tool-call
// arg into its string value, so a path arrives double-quoted and a bare literal
// passes through as-is.
func TestAgyArgString(t *testing.T) {
	require.Equal(t, "/abs/path", agyArgString(`"/abs/path"`), "JSON-string value is unquoted")
	require.Equal(t, "5000", agyArgString("5000"), "non-string literal passes through")
	require.Equal(t, "", agyArgString(""), "missing arg stays empty")
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
		{"trust-prompt.txt", agentbackend.StateNeedsInput},
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

// TestAntigravityParseApprovalTrust parses the captured workspace-trust prompt (shown
// when `agy` launches in an untrusted directory, BEFORE any model call) into the
// neutral Approval: the directory under question (Action), the trust header
// (Question), both unnumbered options top-down, the ">"-cursored selection, and the
// sticky affirmative — so the prompt reaches the approvals inbox instead of silently
// stalling the agent.
func TestAntigravityParseApprovalTrust(t *testing.T) {
	a, ok := Antigravity{}.ParseApproval(agyFixture(t, "trust-prompt.txt"))
	require.True(t, ok, "the captured workspace-trust prompt parses")

	require.Equal(t, "Do you trust the contents of this project?", a.Question)
	require.True(t, strings.HasSuffix(a.Action, "/agytrust.dHzcZl"),
		"Action is the workspace path under the 'Accessing workspace:' label, got %q", a.Action)
	require.Equal(t, []string{"Yes, I trust this folder", "No, exit"}, a.Options)
	require.Equal(t, 1, a.SelectedIdx, "the > cursor sits on the trust option")
	require.Equal(t, 1, a.AffirmativeIdx)
	require.True(t, a.AffirmativeSticky, "trusting the folder is a standing grant")
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

// --- Model menu (live `agy models`) -----------------------------------------

// TestParseAgyModels locks the parser against the REAL `agy models` output captured
// live (testdata/antigravity/models.txt, agy v1.0.13): one model per line, no header,
// blanks dropped, order preserved. The ids are the exact `--model` labels.
func TestParseAgyModels(t *testing.T) {
	out := agyFixture(t, "models.txt")
	got := parseAgyModels([]byte(out))
	want := []string{
		"Gemini 3.5 Flash (Medium)",
		"Gemini 3.5 Flash (High)",
		"Gemini 3.5 Flash (Low)",
		"Gemini 3.1 Pro (Low)",
		"Gemini 3.1 Pro (High)",
		"Claude Sonnet 4.6 (Thinking)",
		"Claude Opus 4.6 (Thinking)",
		"GPT-OSS 120B (Medium)",
	}
	require.Equal(t, want, got)
}

// TestParseAgyModelsTrimsAndDropsBlanks covers the edge shapes the trim/drop handles:
// surrounding whitespace, blank lines, a trailing newline. Never nil so JSON emits [].
func TestParseAgyModelsTrimsAndDropsBlanks(t *testing.T) {
	require.Equal(t, []string{"a", "b"}, parseAgyModels([]byte("  a  \n\n b\n")))
	require.Equal(t, []string{}, parseAgyModels(nil), "empty input ⇒ empty (non-nil) slice")
}

// TestAntigravityListModels drives ListModels through the stubbed `agy models` runner
// (the fixture stands in for the live binary) and asserts it normalizes the menu.
func TestAntigravityListModels(t *testing.T) {
	orig := agyModelsCmd
	t.Cleanup(func() { agyModelsCmd = orig })
	agyModelsCmd = func() ([]byte, error) {
		return []byte(agyFixture(t, "models.txt")), nil
	}
	models, ok := Antigravity{}.ListModels()
	require.True(t, ok)
	require.Len(t, models, 8)
	require.Equal(t, "Gemini 3.5 Flash (Medium)", models[0])
	require.Equal(t, "GPT-OSS 120B (Medium)", models[len(models)-1])
}

// TestAntigravityListModelsDegradesOnError: a command error (binary missing / not
// signed in) returns ok=false so `wd models` reports a clean degrade, never a crash.
func TestAntigravityListModelsDegradesOnError(t *testing.T) {
	orig := agyModelsCmd
	t.Cleanup(func() { agyModelsCmd = orig })
	agyModelsCmd = func() ([]byte, error) {
		return nil, errors.New("exec: agy: not found")
	}
	_, ok := Antigravity{}.ListModels()
	require.False(t, ok, "a command error degrades to ok=false")
}

// --- Usage limits (UsageLimiter) --------------------------------------------

func TestAntigravityFetchUsageUnauthenticated(t *testing.T) {
	orig := agyTokenPath
	t.Cleanup(func() { agyTokenPath = orig })
	agyTokenPath = func() string { return "/nonexistent/token" }

	got, ok := Antigravity{}.FetchUsage(context.Background())
	require.True(t, ok)
	require.Equal(t, "unauthenticated", got.Status)
	require.Empty(t, got.Usage)
}

func TestAntigravityFetchUsageDualWindow(t *testing.T) {
	raw, err := os.ReadFile("../../backendusage/testdata/antigravity-available-models.json")
	require.NoError(t, err)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"refreshed_token"}`)
	}))
	defer tokenSrv.Close()

	modelsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer refreshed_token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer modelsSrv.Close()

	origTokenPath := agyTokenPath
	origReadFile := agyReadFile
	origEndpoint := agyEndpoint
	origTokenURL := agyTokenURL
	t.Cleanup(func() {
		agyTokenPath = origTokenPath
		agyReadFile = origReadFile
		agyEndpoint = origEndpoint
		agyTokenURL = origTokenURL
	})

	agyTokenPath = func() string { return "/test/token" }
	agyReadFile = func(string) ([]byte, error) {
		return []byte(`{
			"token": {
				"access_token": "expired",
				"refresh_token": "valid_refresh",
				"expiry": "2026-09-01T00:00:00Z"
			},
			"auth_method": "consumer"
		}`), nil
	}
	agyEndpoint = modelsSrv.URL
	agyTokenURL = tokenSrv.URL

	got, ok := Antigravity{}.FetchUsage(context.Background())
	require.True(t, ok)
	require.Equal(t, "ok", got.Status)
	require.NotNil(t, got.Account)
	require.Equal(t, "Free Tier", got.Account.Plan)
	require.Len(t, got.Usage, 2)
	require.Equal(t, "antigravity:gemini", got.Usage[0].ID)
	require.InDelta(t, 32.03, *got.Usage[0].UsedPercent, 0.01)
	require.Equal(t, "antigravity:non-gemini", got.Usage[1].ID)
	require.InDelta(t, 0.0, *got.Usage[1].UsedPercent, 0.01)
}
