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

// --- State / approval (degraded) --------------------------------------------

func TestCodexStateDegrades(t *testing.T) {
	require.Equal(t, agentbackend.StateUnknown, Codex{}.DetectState("any pane content"))
	_, ok := Codex{}.ParseApproval("Allow command? [y/n]")
	require.False(t, ok, "interactive approval parsing is deferred — degrade, not mis-parse")
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
