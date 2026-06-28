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

func TestCodexLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "model + default mode (Codex's own posture applies)",
			opts: agentbackend.LaunchOpts{Model: "qwen2.5-coder:3b", Mode: "default"},
			want: "codex -m 'qwen2.5-coder:3b'",
		},
		{
			name: "read-only sandbox passes through",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "read-only"},
			want: "codex -m 'm' -s read-only",
		},
		{
			name: "workspace-write sandbox passes through",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "workspace-write"},
			want: "codex -m 'm' -s workspace-write",
		},
		{
			name: "danger-full-access also pins -a never",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "danger-full-access"},
			want: "codex -m 'm' -s danger-full-access -a never",
		},
		{
			name: "claude 'dangerously-skip-permissions' folds onto danger-full-access",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "dangerously-skip-permissions"},
			want: "codex -m 'm' -s danger-full-access -a never",
		},
		{
			name: "empty model omits -m (BYO config provider)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "codex",
		},
		{
			name: "session id and name are ignored (SessionIDControl=false)",
			opts: agentbackend.LaunchOpts{SessionID: "uuid", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "codex -m 'm'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Codex{}.LaunchCmd(tt.opts))
		})
	}
}

// TestCodexLaunchQuotesModel is the command-injection guard: a model string with
// shell metacharacters must be single-quoted so it cannot break out of the line
// typed into a tmux pane.
func TestCodexLaunchQuotesModel(t *testing.T) {
	got := Codex{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m; touch /tmp/pwned #", Mode: "default"})
	require.Equal(t, "codex -m 'm; touch /tmp/pwned #'", got)
}

func TestCodexResumeCmd(t *testing.T) {
	// Codex mints its own UUID id warden cannot pin, so resume is dir-scoped --last.
	cmd, ok := Codex{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "whatever", Model: "m"})
	require.True(t, ok, "Codex supports resume (Caps.Resume=true)")
	require.Equal(t, "codex resume --last", cmd)
}

func TestCodexLaunchPromptArg(t *testing.T) {
	got := Codex{}.LaunchPromptArg("/state/prompts/job-1")
	require.Equal(t, ` "$(cat '/state/prompts/job-1')"`, got)
}

func TestCodexHeadlessCmd(t *testing.T) {
	argv, ok := Codex{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"codex", "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "classify this"}, argv)
}

// --- Transcript resolution (dir-scoped) -------------------------------------

// TestCodexTranscriptPathDirScoped points CODEX_HOME at the fixture session tree
// and resolves the rollout by matching session_meta.cwd to the agent's workdir.
func TestCodexTranscriptPathDirScoped(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	p, ok := Codex{}.TranscriptPath("", "/work/agent-codex", "warden-placeholder-uuid")
	require.True(t, ok, "rollout for the workdir resolves")
	require.True(t, strings.HasSuffix(p, ".jsonl"))
	require.Contains(t, p, filepath.Join("sessions", "2026", "06", "28"))
}

func TestCodexTranscriptPathDegrades(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	// No workdir ⇒ nothing to resolve.
	_, ok := Codex{}.TranscriptPath("", "", "")
	require.False(t, ok)

	// A directory with no matching rollout ⇒ degrade (digest shows "no transcript").
	_, ok = Codex{}.TranscriptPath("", "/work/no-such-agent", "")
	require.False(t, ok)

	// A CODEX_HOME with no sessions tree at all ⇒ degrade, no error.
	t.Setenv("CODEX_HOME", t.TempDir())
	_, ok = Codex{}.TranscriptPath("", "/work/agent-codex", "")
	require.False(t, ok)
}

// --- Discover-then-pin session id -------------------------------------------

// TestCodexDiscoverSessionID points CODEX_HOME at the fixture tree and asserts the
// dir-scoped locator finds the agent's rollout and extracts its minted session id
// from the `session_meta` header — the id warden pins post-launch (discover-then-pin).
func TestCodexDiscoverSessionID(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	id, ok := Codex{}.DiscoverSessionID("", "/work/agent-codex")
	require.True(t, ok, "session id discovered from the workdir's rollout")
	require.Equal(t, "019f0d8c-6c54-7471-af1c-a9043b9f11e0", id)
}

// TestCodexDiscoverSessionIDDegrades covers the misses: no workdir, a workdir with
// no rollout, and a sessions tree that exists but whose header carries no id — each
// returns ok=false so the poller keeps the empty id and retries on a later tick.
func TestCodexDiscoverSessionIDDegrades(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join("testdata", "codex"))

	_, ok := Codex{}.DiscoverSessionID("", "")
	require.False(t, ok, "no workdir ⇒ nothing to resolve")

	_, ok = Codex{}.DiscoverSessionID("", "/work/no-such-agent")
	require.False(t, ok, "no rollout for the dir ⇒ degrade")

	// A rollout whose session_meta header carries no id ⇒ degrade rather than pin "".
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "06", "28")
	require.NoError(t, os.MkdirAll(day, 0o755))
	roll := filepath.Join(day, "rollout-2026-06-28T00-00-00-noid.jsonl")
	require.NoError(t, os.WriteFile(roll,
		[]byte(`{"type":"session_meta","payload":{"cwd":"/work/noid"}}`+"\n"), 0o644))
	t.Setenv("CODEX_HOME", home)
	_, ok = Codex{}.DiscoverSessionID("", "/work/noid")
	require.False(t, ok, "header without a session id ⇒ degrade")
}

// codexRolloutSessionID also accepts the older rollouts that carry only `id` (no
// `session_id`) in the header.
func TestCodexDiscoverSessionIDLegacyIDField(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "06", "28")
	require.NoError(t, os.MkdirAll(day, 0o755))
	roll := filepath.Join(day, "rollout-2026-06-28T00-00-00-legacy.jsonl")
	require.NoError(t, os.WriteFile(roll,
		[]byte(`{"type":"session_meta","payload":{"id":"legacy-uuid-1234","cwd":"/work/legacy"}}`+"\n"), 0o644))
	t.Setenv("CODEX_HOME", home)

	id, ok := Codex{}.DiscoverSessionID("", "/work/legacy")
	require.True(t, ok)
	require.Equal(t, "legacy-uuid-1234", id)
}

// --- Transcript parsing -----------------------------------------------------

// TestCodexParseTranscript parses the real captured rollout fixture and asserts the
// neutral Turns warden's digest depends on: exactly one human prompt and one model
// reply, with Codex's synthetic <environment_context>/developer messages dropped.
func TestCodexParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "codex", "sessions", "2026", "06", "28",
		"rollout-2026-06-28T11-25-34-019f0d8c-6c54-7471-af1c-a9043b9f11e0.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Codex{}.ParseTranscript(f)
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
	require.Equal(t, 1, users, "synthetic env-context / developer user messages are dropped")
	require.Equal(t, 1, assistants)

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "add(a, b)")
	require.False(t, turns[0].Timestamp.IsZero(), "rollout timestamp applied")

	require.Equal(t, "assistant", turns[1].Role)
	require.Contains(t, turns[1].Text, `"name": "add"`)
}

// TestCodexParseTranscriptToolCall covers a function_call (apply_patch): the tool
// name and the patched file fold onto the preceding assistant turn. The 3B local
// fixture model never emitted a tool call, so this fixture is hand-authored from
// the Codex rollout schema (mirroring OpenCode's export-tool.json).
func TestCodexParseTranscriptToolCall(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "codex", "rollout-toolcall.jsonl"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Codex{}.ParseTranscript(f)
	require.NoError(t, err)
	require.Len(t, turns, 2, "the function_call folds onto the assistant turn; output is ignored")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "subtract")

	a := turns[1]
	require.Equal(t, "assistant", a.Role)
	require.Contains(t, a.Text, "I'll add a subtract")
	require.Equal(t, "apply_patch", a.ToolName)
	require.Equal(t, []string{"calc.py"}, a.Files, "files parsed from the apply_patch envelope")
}

// TestCodexParseTranscriptShellPatch covers the shell-tool route: an apply_patch
// run via the `shell` tool's command array still yields the touched file.
func TestCodexParseTranscriptShellPatch(t *testing.T) {
	stream := strings.Join([]string{
		`{"timestamp":"2026-06-28T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"edit calc.py"}]}}`,
		`{"timestamp":"2026-06-28T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"apply_patch <<'EOF'\\n*** Begin Patch\\n*** Update File: calc.py\\n@@\\n+x=1\\n*** End Patch\\nEOF\"]}"}}`,
	}, "\n")

	turns, err := Codex{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 2)
	require.Equal(t, "assistant", turns[1].Role, "a tool call with no preceding assistant text starts a new turn")
	require.Equal(t, "shell", turns[1].ToolName)
	require.Equal(t, []string{"calc.py"}, turns[1].Files)
}

// TestCodexParseTranscriptTolerant skips malformed lines and ignores
// event_msg/turn_context/session_meta records rather than erroring.
func TestCodexParseTranscriptTolerant(t *testing.T) {
	stream := strings.Join([]string{
		`not json at all`,
		`{"type":"session_meta","payload":{"cwd":"/x"}}`,
		`{"type":"event_msg","payload":{"type":"task_started"}}`,
		`{"timestamp":"2026-06-28T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		``,
	}, "\n")

	turns, err := Codex{}.ParseTranscript(strings.NewReader(stream))
	require.NoError(t, err)
	require.Len(t, turns, 1)
	require.Equal(t, "hello", turns[0].Text)
}

// --- State / approval (live markers) ----------------------------------------

// codexFixture reads a captured tmux-pane fixture from testdata/codex/.
func codexFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex", name))
	require.NoError(t, err)
	return string(b)
}

// TestCodexDetectState classifies each captured pane: streaming ⇒ Working (the
// "esc to interrupt" marker), an open approval ⇒ NeedsInput, an at-rest pane ⇒
// Unknown (Codex has no positive idle marker; warden infers idle from staleness).
func TestCodexDetectState(t *testing.T) {
	tests := []struct {
		fixture string
		want    agentbackend.State
	}{
		{"state-working.txt", agentbackend.StateWorking},
		{"approval-command.txt", agentbackend.StateNeedsInput},
		{"state-idle.txt", agentbackend.StateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			require.Equal(t, tt.want, Codex{}.DetectState(codexFixture(t, tt.fixture)))
		})
	}

	// An unrecognized pane stays Unknown (no false positive).
	require.Equal(t, agentbackend.StateUnknown, Codex{}.DetectState("just some quiet output"))
}

// TestCodexParseApproval parses the captured command-escalation approval into the
// neutral Approval: the proposed command (Action), the "Would you like to …?"
// header (Question), the three options top-down (1-indexed), the highlighted
// option (SelectedIdx), and the least-privilege non-sticky "Yes" (AffirmativeIdx).
func TestCodexParseApproval(t *testing.T) {
	a, ok := Codex{}.ParseApproval(codexFixture(t, "approval-command.txt"))
	require.True(t, ok, "the captured approval prompt parses")

	require.Equal(t, "curl -sI https://example.com", a.Action)
	require.Equal(t, "Would you like to run the following command?", a.Question)
	require.Equal(t, []string{
		"Yes, proceed (y)",
		"Yes, and don't ask again for commands that start with `curl -sI` (p)",
		"No, and tell Codex what to do differently (esc)",
	}, a.Options)
	require.Equal(t, 1, a.SelectedIdx, "the › cursor sits on option 1")
	require.Equal(t, 1, a.AffirmativeIdx, "least-privilege affirmative is the non-sticky Yes")
	require.False(t, a.AffirmativeSticky, "option 1 is a one-shot grant, not a standing one")
}

// TestCodexParseApprovalNegative proves a non-approval pane (idle or working) is
// NOT mis-read as an approval — the header gate keeps the auto-approve path honest.
func TestCodexParseApprovalNegative(t *testing.T) {
	for _, name := range []string{"state-idle.txt", "state-working.txt"} {
		t.Run(name, func(t *testing.T) {
			_, ok := Codex{}.ParseApproval(codexFixture(t, name))
			require.False(t, ok)
		})
	}

	// A bare numbered list in agent prose (no "Would you like to" header) is not an
	// approval, even though it has sequential 1..N lines.
	prose := "Here are the steps:\n  1. Yes do this\n  2. No skip that\n"
	_, ok := Codex{}.ParseApproval(prose)
	require.False(t, ok, "a numbered list without the approval header is not a prompt")
}

// TestCodexAffirmativeStickyFallback covers the case where the only affirmative is
// a standing "don't ask again" grant: it is chosen with sticky=true.
func TestCodexAffirmativeStickyFallback(t *testing.T) {
	idx, sticky := codexAffirmative([]string{
		"Yes, and don't ask again for this session",
		"No, stop",
	})
	require.Equal(t, 1, idx)
	require.True(t, sticky)

	idx, sticky = codexAffirmative([]string{"No, cancel", "No, and tell Codex what to do"})
	require.Equal(t, 0, idx, "no affirmative offered")
	require.False(t, sticky)
}

// --- Capabilities / pricing -------------------------------------------------

func TestCodexCapabilities(t *testing.T) {
	c := Codex{}.Capabilities()
	require.True(t, c.Resume, "Codex resumes (codex resume --last)")
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.True(t, c.StructuredTranscript, "Tier A: rollout JSONL parses into Turns")
	require.False(t, c.SessionIDControl, "Codex mints its own UUID session id")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"read-only", "workspace-write", "danger-full-access"}, c.PermissionModes)
}

func TestCodexNoPricing(t *testing.T) {
	_, ok := Codex{}.Pricing()
	require.False(t, ok, "OSS/BYO ⇒ no warden-side dollar pricing table yet")
}

func TestCodexSystemPromptUnsupported(t *testing.T) {
	_, ok := Codex{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

func TestCodexRegistered(t *testing.T) {
	b, err := agentbackend.Get("codex")
	require.NoError(t, err)
	require.Equal(t, "Codex", b.DisplayName())
	require.Equal(t, "codex", b.Binary())
}
