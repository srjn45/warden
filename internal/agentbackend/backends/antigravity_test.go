package backends

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
			name: "model + default mode defaults to --dangerously-skip-permissions",
			opts: agentbackend.LaunchOpts{Model: "Gemini 3.5 Flash (Low)", Mode: "default"},
			want: "agy --model 'Gemini 3.5 Flash (Low)' --dangerously-skip-permissions",
		},
		{
			name: "plan maps to --mode plan",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "plan"},
			want: "agy --model 'm' --mode plan",
		},
		{
			name: "accept-edits maps to --mode accept-edits",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "accept-edits"},
			want: "agy --model 'm' --mode accept-edits",
		},
		{
			name: "claude acceptEdits maps to --mode accept-edits",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "acceptEdits"},
			want: "agy --model 'm' --mode accept-edits",
		},
		{
			name: "sandbox mode does not emit --sandbox (universal full network)",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "sandbox"},
			want: "agy --model 'm'",
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
			want: "agy --dangerously-skip-permissions",
		},
		{
			name: "session id and name are ignored (SessionIDControl=false)",
			opts: agentbackend.LaunchOpts{SessionID: "uuid", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "agy --model 'm' --dangerously-skip-permissions",
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
	require.Equal(t, "agy --model 'm; touch /tmp/pwned #' --dangerously-skip-permissions", got)
}

func TestAntigravityResumeCmd(t *testing.T) {
	// agy mints its own UUID id warden cannot pin (and warden's placeholder is also a
	// UUID), so resume is dir-scoped `agy -c`.
	cmd, ok := Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "whatever", Model: "m"})
	require.True(t, ok, "Antigravity supports resume (Caps.Resume=true)")
	require.Equal(t, "agy -c --model 'm' --dangerously-skip-permissions", cmd)

	// Permission mode flows through resume too.
	cmd, _ = Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{Mode: "dangerously-skip-permissions"})
	require.Equal(t, "agy -c --dangerously-skip-permissions", cmd)

	cmd, _ = Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{Mode: "plan"})
	require.Equal(t, "agy -c --mode plan", cmd)
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
		{"approval-run-command.txt", agentbackend.StateNeedsInput},
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

// TestAntigravityParseApprovalRunCommand parses the reworded prompt `agy` v1.2.17
// shows ("Run this command?" / "Yes, run command" / "No, cancel"). The old parser
// required the "Do you want to proceed?" header, so this prompt was never recognized:
// the agent stayed "working" and auto-approve never answered it.
func TestAntigravityParseApprovalRunCommand(t *testing.T) {
	a, ok := Antigravity{}.ParseApproval(agyFixture(t, "approval-run-command.txt"))
	require.True(t, ok, "the reworded permission prompt parses")

	require.Equal(t, "git rev-parse HEAD origin/main", a.Action)
	require.Equal(t, "Run this command?", a.Question)
	require.Equal(t, []string{
		"Yes, run command",
		"Yes, and always allow in this conversation for commands that start with 'git rev-parse'",
		"Yes, and always allow for commands that start with 'git rev-parse' (Persist to settings.json)",
		"No, cancel",
	}, a.Options)
	require.Equal(t, 1, a.SelectedIdx)
	require.Equal(t, 1, a.AffirmativeIdx, "least-privilege affirmative is the one-shot Yes")
	require.False(t, a.AffirmativeSticky)
	require.Empty(t, a.Kind, "a command prompt is not a trust prompt")
	require.False(t, a.Navigate, "numbered options are answered by their digit")
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

	// Nor is a numbered list that merely follows a question in prose: without the
	// "Requesting permission for:" label an unknown question is not a prompt.
	asked := "Which do you prefer?\n  1. Yes do this\n  2. No skip that\n"
	_, ok = Antigravity{}.ParseApproval(asked)
	require.False(t, ok, "a question plus a numbered list is not a permission prompt")
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
	require.Equal(t, []string{"default", "plan", "accept-edits", "sandbox", "dangerously-skip-permissions"}, c.PermissionModes)
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

func TestAntigravityFetchUsageTwoBuckets(t *testing.T) {
	raw, err := os.ReadFile("../../backendusage/testdata/antigravity-quota-summary.json")
	require.NoError(t, err)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"refreshed_token"}`)
	}))
	defer tokenSrv.Close()

	modelsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer refreshed_token", r.Header.Get("Authorization"))
		require.Contains(t, r.URL.Path, "retrieveUserQuotaSummary")
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
	require.Equal(t, "rate_limited", got.Status)
	require.NotNil(t, got.Account)
	require.Equal(t, "Free Tier", got.Account.Plan)
	require.Len(t, got.Usage, 2)

	// Gemini: weekly still has headroom, reports the (exhausted) 5-hour limit.
	require.Equal(t, "antigravity:gemini", got.Usage[0].ID)
	require.Equal(t, "gemini", got.Usage[0].Scope)
	require.Equal(t, float64(100), *got.Usage[0].UsedPercent)
	require.Equal(t, float64(0), *got.Usage[0].RemainingPercent)
	require.Equal(t, agyFiveHourMinutes, *got.Usage[0].DurationMinutes)
	require.Equal(t, "reached", *got.Usage[0].LimitState)
	expected5h, _ := time.Parse(time.RFC3339, "2026-09-05T21:40:45Z")
	require.Equal(t, expected5h.UTC(), *got.Usage[0].ResetsAt)

	// Non-Gemini: weekly has headroom, reports the untouched 5-hour limit.
	require.Equal(t, "antigravity:non-gemini", got.Usage[1].ID)
	require.Equal(t, "non-gemini", got.Usage[1].Scope)
	require.InDelta(t, 0.0, *got.Usage[1].UsedPercent, 0.01)
	require.InDelta(t, 100.0, *got.Usage[1].RemainingPercent, 0.01)
	require.Nil(t, got.Usage[1].LimitState)
}

// TestAgyParseQuotaSummaryWeeklyExhaustion asserts the weekly-exhaustion
// override in the agentbackend adapter matches the backendusage adapter: once a
// pool's weekly limit is drained (remainingFraction <= 0), its bucket reports
// the weekly limit fully consumed even while the 5-hour bucket has headroom.
func TestAgyParseQuotaSummaryWeeklyExhaustion(t *testing.T) {
	body := []byte(`{
		"groups": [
			{
				"displayName": "Gemini Models",
				"buckets": [
					{"bucketId": "gemini-weekly", "window": "weekly", "resetTime": "2026-09-12T08:00:00Z", "remainingFraction": 0},
					{"bucketId": "gemini-5h", "window": "5h", "resetTime": "2026-09-06T01:00:00Z", "remainingFraction": 0.5}
				]
			},
			{
				"displayName": "Claude and GPT models",
				"buckets": [
					{"bucketId": "3p-weekly", "window": "weekly", "resetTime": "2026-09-12T09:00:00Z", "remainingFraction": 0.9},
					{"bucketId": "3p-5h", "window": "5h", "resetTime": "2026-09-06T02:00:00Z", "remainingFraction": 0.25}
				]
			}
		]
	}`)

	limits, ok := agyParseQuotaSummary(body)
	require.True(t, ok)
	require.Len(t, limits, 2)

	require.Equal(t, "antigravity:gemini", limits[0].ID)
	require.Equal(t, float64(100), *limits[0].UsedPercent)
	require.Equal(t, "reached", *limits[0].LimitState)
	require.Equal(t, agyWeeklyMinutes, *limits[0].DurationMinutes)
	expectedWeekly, _ := time.Parse(time.RFC3339, "2026-09-12T08:00:00Z")
	require.Equal(t, expectedWeekly.UTC(), *limits[0].ResetsAt)

	require.Equal(t, "antigravity:non-gemini", limits[1].ID)
	require.InDelta(t, 75.0, *limits[1].UsedPercent, 0.01)
	require.Nil(t, limits[1].LimitState)
	require.Equal(t, agyFiveHourMinutes, *limits[1].DurationMinutes)
}

// --- Rate-limit detection (provider-specific banner) ------------------------

// sampleAgyRateLimitBanner is a plausible agy TUI rate-limit banner: it carries
// both a session-quota limit phrase and an "available at HH:MM" time clause.
// Keep in sync with antigravityRLBannerRe.
//
// TODO(confirm-wording): replace with the VERBATIM banner string captured from
// a live agy rate-limit hit. Until then the trailing-window anchor keeps
// behavior fail-closed.
const sampleAgyRateLimitBanner = "⚠ Free-tier session quota reached.\n  Available at 15:30 · gemini.google.com/settings"

// TestAntigravityDetectRateLimit_FixtureBanner reads the captured rate-limit
// fixture (rate-limit.txt) and confirms the detector fires — banner in tail,
// reset time parsed, isLimited=true.
func TestAntigravityDetectRateLimit_FixtureBanner(t *testing.T) {
	pane := agyFixture(t, "rate-limit.txt")
	limited, restore, ok := Antigravity{}.DetectRateLimit(pane)
	require.True(t, limited, "rate-limit fixture must be detected")
	require.True(t, ok, "reset time must parse from the banner")
	require.False(t, restore.IsZero(), "parsed restore time must be non-zero")
	require.True(t, restore.After(time.Now().Add(-25*time.Hour)),
		"restore time must be within the next 24 h window")
}

// TestAntigravityDetectRateLimit_InlineBanner covers the sampleAgyRateLimitBanner
// constant used in sibling tests, so a future wording change breaks here first.
func TestAntigravityDetectRateLimit_InlineBanner(t *testing.T) {
	limited, _, ok := Antigravity{}.DetectRateLimit("noise\n" + sampleAgyRateLimitBanner)
	require.True(t, limited)
	require.True(t, ok)
}

// TestAntigravityDetectRateLimit_BannerScrolledAway ensures the detector does
// NOT fire when the banner has scrolled above the trailing window — stale pane
// content must not produce a false rate-limit classification.
func TestAntigravityDetectRateLimit_BannerScrolledAway(t *testing.T) {
	// Banner in the past; 20 lines of normal work push it out of the 6-line tail.
	pane := sampleAgyRateLimitBanner + strings.Repeat("\nnormal work line", 20)
	limited, _, _ := Antigravity{}.DetectRateLimit(pane)
	require.False(t, limited, "banner outside the trailing window must not match")
}

// TestAntigravityDetectRateLimit_NegativeAgentProse verifies that ordinary
// agent prose mentioning quota, rate limit, or limit reached does NOT trigger
// detection — only the structured banner (limit phrase + reset time) matches.
func TestAntigravityDetectRateLimit_NegativeAgentProse(t *testing.T) {
	cases := []struct {
		name string
		pane string
	}{
		{
			name: "agent discusses rate limiting in code",
			pane: "func handleRateLimit() {\n  // check quota before calling API\n  if limit reached { return err }\n}\n❯ esc to interrupt",
		},
		{
			name: "agent mentions quota in explanation",
			pane: "The API enforces a quota of 100 requests per minute. If you exceed the rate limit, the server returns 429.\n? for shortcuts",
		},
		{
			name: "plain mention of limit reached without time",
			pane: "Error: limit reached. Please try again later.\n? for shortcuts",
		},
		{
			name: "agent discusses resource exhausted in documentation",
			pane: "Resource exhausted errors occur when you exceed the quota for a Google Cloud API.\n? for shortcuts",
		},
		{
			name: "conversation transcript containing quota discussion",
			pane: "User: what happens when quota exceeded?\nAssistant: The API returns a 429 rate limited response.\n? for shortcuts",
		},
		{
			name: "bare available at time with no limit phrase",
			pane: "Meeting available at 09:00 in conference room B.\n? for shortcuts",
		},
		{
			name: "bare resets at time with no limit phrase",
			pane: "Cache resets at 00:00 every night.\n? for shortcuts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limited, _, _ := Antigravity{}.DetectRateLimit(tc.pane)
			require.False(t, limited, "ordinary prose must not trigger rate-limit detection")
		})
	}
}

// TestAntigravityDetectRateLimit_WorkingPaneIsNeverLimited verifies that a pane
// with the "esc to cancel" working marker is not classified as rate-limited even
// if it incidentally contains limit-adjacent words, since a streaming agent
// cannot simultaneously be at a rate-limit banner.
func TestAntigravityDetectRateLimit_WorkingPaneIsNeverLimited(t *testing.T) {
	pane := agyFixture(t, "state-working.txt")
	limited, _, _ := Antigravity{}.DetectRateLimit(pane)
	require.False(t, limited, "a live working pane must not match the rate-limit banner")
}

// TestAntigravityDetectRateLimit_IdlePaneIsNeverLimited verifies that the idle
// pane (? for shortcuts footer, normal agent output) does not trigger detection.
func TestAntigravityDetectRateLimit_IdlePaneIsNeverLimited(t *testing.T) {
	pane := agyFixture(t, "state-idle.txt")
	limited, _, _ := Antigravity{}.DetectRateLimit(pane)
	require.False(t, limited, "a normal idle pane must not match the rate-limit banner")
}

// TestAntigravityParseRateLimitReset_ResetsAt covers both "resets at HH:MM"
// and "available at HH:MM" formats, and confirms the generic fallback also
// works for non-agy-specific time formats that agy might use.
func TestAntigravityParseRateLimitReset_ResetsAt(t *testing.T) {
	loc := time.Local
	tests := []struct {
		name     string
		pane     string
		wantHour int
		wantMin  int
	}{
		{"resets at 24h", "quota reached. resets at 15:30", 15, 30},
		{"available at 24h", "session limit. available at 09:00", 9, 0},
		{"available at 12h am", "usage limit reached. available at 9:30am", 9, 30},
		{"available at 12h pm", "quota exhausted. available at 3:45pm", 15, 45},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Antigravity{}.ParseRateLimitReset(tt.pane)
			require.True(t, ok, "reset time must parse")
			require.Equal(t, tt.wantHour, got.In(loc).Hour())
			require.Equal(t, tt.wantMin, got.In(loc).Minute())
			require.True(t, got.After(time.Now().Add(-25*time.Hour)))
		})
	}
}

// TestAntigravityParseRateLimitReset_NoTime verifies that a pane with no clock
// time returns ok=false — the scheduler then applies the fallback retry interval.
func TestAntigravityParseRateLimitReset_NoTime(t *testing.T) {
	_, ok := Antigravity{}.ParseRateLimitReset("quota exceeded, try again later")
	require.False(t, ok, "pane with no clock time must return ok=false")
}

// --- Per-session conversation pinning (#686) --------------------------------

const (
	agyConvA = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	agyConvB = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
)

// agyHomeWithConvs builds a throwaway agyHome holding one transcript per conv id and
// a last_conversations.json mapping workdir -> mapped (the single dir-scoped entry).
func agyHomeWithConvs(t *testing.T, workdir, mapped string, convs ...string) string {
	t.Helper()
	home := t.TempDir()
	for _, c := range convs {
		p := filepath.Join(home, "brain", c, agyTranscriptRel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("{}\n"), 0o644))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(home, "cache"), 0o755))
	m, err := json.Marshal(map[string]string{workdir: mapped})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "cache", "last_conversations.json"), m, 0o644))
	withAgyHome(t, home)
	return home
}

// Two sessions share one workdir: the dir map holds only the last writer, but each
// pinned session resolves its OWN conversation.
func TestAntigravityTranscriptPathPinnedPerSession(t *testing.T) {
	home := agyHomeWithConvs(t, "/work/shared", agyConvB, agyConvA, agyConvB)

	pa, ok := Antigravity{}.TranscriptPath("", "/work/shared", agyConvA)
	require.True(t, ok)
	pb, ok := Antigravity{}.TranscriptPath("", "/work/shared", agyConvB)
	require.True(t, ok)
	require.NotEqual(t, pa, pb)
	require.Equal(t, filepath.Join(home, "brain", agyConvA, agyTranscriptRel), pa)
	require.Equal(t, filepath.Join(home, "brain", agyConvB, agyTranscriptRel), pb)

	// A pinned id whose transcript does not exist degrades; it never falls back to
	// the directory's entry (that is another session's conversation).
	_, ok = Antigravity{}.TranscriptPath("", "/work/shared", "cccccccc-3333-4333-8333-cccccccccccc")
	require.False(t, ok)
}

// A legacy record (no pinned id) keeps the dir-scoped resolution.
func TestAntigravityTranscriptPathLegacyUnpinned(t *testing.T) {
	home := agyHomeWithConvs(t, "/work/solo", agyConvA, agyConvA)
	p, ok := Antigravity{}.TranscriptPath("", "/work/solo", "")
	require.True(t, ok)
	require.Equal(t, filepath.Join(home, "brain", agyConvA, agyTranscriptRel), p)
}

func TestAntigravityLaunchCmdLogFile(t *testing.T) {
	require.Equal(t, "agy --log-file '/d/a1.log' --dangerously-skip-permissions",
		Antigravity{}.LaunchCmd(agentbackend.LaunchOpts{LogFile: "/d/a1.log"}))
	require.Equal(t, "agy --dangerously-skip-permissions", Antigravity{}.LaunchCmd(agentbackend.LaunchOpts{}))
}

func TestAntigravityResumeCmdPinned(t *testing.T) {
	cmd, ok := Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: agyConvA})
	require.True(t, ok)
	require.Equal(t, "agy --conversation "+agyConvA+" --dangerously-skip-permissions", cmd)
	// Unpinned (legacy / not yet discovered) keeps `-c`.
	cmd, _ = Antigravity{}.ResumeCmd(agentbackend.ResumeOpts{})
	require.Equal(t, "agy -c --dangerously-skip-permissions", cmd)
}

func TestAntigravityDiscoverSessionIDFromLog(t *testing.T) {
	dir := t.TempDir()
	_, ok := Antigravity{}.DiscoverSessionIDFromLog(filepath.Join(dir, "missing.log"))
	require.False(t, ok)

	log := filepath.Join(dir, "a.log")
	require.NoError(t, os.WriteFile(log, []byte("I1005 server.go:1] starting\n"), 0o644))
	_, ok = Antigravity{}.DiscoverSessionIDFromLog(log)
	require.False(t, ok, "no conversation created yet")

	require.NoError(t, os.WriteFile(log, []byte(
		"I1005 server.go:1263] Created conversation "+agyConvA+"\nI1005 server.go:1263] Created conversation "+agyConvB+"\n"), 0o644))
	id, ok := Antigravity{}.DiscoverSessionIDFromLog(log)
	require.True(t, ok)
	require.Equal(t, agyConvA, id, "first created conversation is the session's own")
}
