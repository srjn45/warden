package backends

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// --- Command builders -------------------------------------------------------

func TestOpenCodeLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "model + default (prompt) mode",
			opts: agentbackend.LaunchOpts{Model: "ollama/qwen2.5-coder:3b", Mode: "default"},
			want: "opencode -m 'ollama/qwen2.5-coder:3b'",
		},
		{
			name: "skip-permissions mode prepends the auto-approve env (TUI has no flag)",
			opts: agentbackend.LaunchOpts{Model: "anthropic/claude-sonnet-4-6", Mode: "dangerously-skip-permissions"},
			want: opencodeAutoApproveEnv + " opencode -m 'anthropic/claude-sonnet-4-6'",
		},
		{
			name: "claude 'auto' folds onto the auto-approve env",
			opts: agentbackend.LaunchOpts{Model: "m", Mode: "auto"},
			want: opencodeAutoApproveEnv + " opencode -m 'm'",
		},
		{
			name: "empty model omits -m (BYO model)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "opencode",
		},
		{
			name: "session id and name are ignored (SessionIDControl=false)",
			opts: agentbackend.LaunchOpts{SessionID: "ses_abc", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "opencode -m 'm'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, OpenCode{}.LaunchCmd(tt.opts))
		})
	}
}

// TestOpenCodeLaunchQuotesModel is the command-injection guard: a model string
// with shell metacharacters must be single-quoted so it cannot break out of the
// line typed into a tmux pane.
func TestOpenCodeLaunchQuotesModel(t *testing.T) {
	got := OpenCode{}.LaunchCmd(agentbackend.LaunchOpts{Model: "m; touch /tmp/pwned #", Mode: "default"})
	require.Equal(t, "opencode -m 'm; touch /tmp/pwned #'", got)
}

func TestOpenCodeResumeCmd(t *testing.T) {
	// No real ses_ id pinned (warden's placeholder UUID) ⇒ dir-scoped -c.
	cmd, ok := OpenCode{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "550e8400-e29b-41d4-a716-446655440000", Model: "m"})
	require.True(t, ok, "OpenCode supports resume (Caps.Resume=true)")
	require.Equal(t, "opencode -c -m 'm'", cmd)

	// A real ses_ id pinned (future discover-then-pin) ⇒ exact-id -s.
	cmd, ok = OpenCode{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "ses_0f46b328fffe5NEQ", Model: "m"})
	require.True(t, ok)
	require.Equal(t, "opencode -s 'ses_0f46b328fffe5NEQ' -m 'm'", cmd)

	// Empty model omits -m.
	cmd, ok = OpenCode{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "x"})
	require.True(t, ok)
	require.Equal(t, "opencode -c", cmd)

	// Skip mode prepends the auto-approve env on resume too.
	cmd, ok = OpenCode{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "x", Model: "m", Mode: "auto"})
	require.True(t, ok)
	require.Equal(t, opencodeAutoApproveEnv+" opencode -c -m 'm'", cmd)
}

func TestOpenCodeLaunchPromptArg(t *testing.T) {
	got := OpenCode{}.LaunchPromptArg("/state/prompts/job-1")
	require.Equal(t, ` --prompt "$(cat '/state/prompts/job-1')"`, got)
}

func TestOpenCodeHeadlessCmd(t *testing.T) {
	argv, ok := OpenCode{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"opencode", "run", "--dangerously-skip-permissions", "classify this"}, argv)
}

func TestLooksLikeSessionID(t *testing.T) {
	require.True(t, looksLikeSessionID("ses_0f46b328fffe5NEQ"))
	require.False(t, looksLikeSessionID("ses_"), "prefix alone is not an id")
	require.False(t, looksLikeSessionID("550e8400-e29b-41d4-a716-446655440000"), "warden UUID is not a ses_ id")
	require.False(t, looksLikeSessionID(""))
}

// --- Session resolution (dir-scoped) ----------------------------------------

func TestNewestSessionForDir(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("testdata", "opencode", "session-list.json"))
	require.NoError(t, err)

	// Dir A has two sessions; the newest (by `updated`) wins.
	id, ok := newestSessionForDir(list, "/work/agent-a")
	require.True(t, ok)
	require.Equal(t, "ses_newerInDirA", id)

	// Dir B has one session.
	id, ok = newestSessionForDir(list, "/work/agent-b")
	require.True(t, ok)
	require.Equal(t, "ses_newerInDirB", id)

	// A directory with no session ⇒ not found (transcript degrades).
	_, ok = newestSessionForDir(list, "/work/unknown")
	require.False(t, ok)

	// Garbage input ⇒ not found, no panic.
	_, ok = newestSessionForDir([]byte("not json"), "/work/agent-a")
	require.False(t, ok)
}

// --- Transcript orchestration (subprocess stubbed) --------------------------

// TestOpenCodeTranscriptPathDirScoped exercises the live path: no real ses_ id
// pinned, so TranscriptPath lists sessions, filters to the workdir, exports the
// newest, and materializes it to a file the caller can open.
func TestOpenCodeTranscriptPathDirScoped(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("testdata", "opencode", "session-list.json"))
	require.NoError(t, err)
	export, err := os.ReadFile(filepath.Join("testdata", "opencode", "export-session.json"))
	require.NoError(t, err)

	var listed, exported string
	restore := stubOcExec(func(_ context.Context, dir string, args ...string) ([]byte, error) {
		switch args[0] {
		case "session":
			listed = dir
			return list, nil
		case "export":
			exported = args[1]
			return export, nil
		}
		return nil, nil
	})
	defer restore()

	p, ok := OpenCode{}.TranscriptPath("", "/work/agent-a", "550e8400-uuid-placeholder")
	require.True(t, ok)
	require.Equal(t, "/work/agent-a", listed, "session list runs in the agent workdir")
	require.Equal(t, "ses_newerInDirA", exported, "exports the newest session for the dir")

	got, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, export, got, "materialized file holds the export JSON")
	_ = os.Remove(p)
}

// TestOpenCodeTranscriptPathPinnedID exercises the forward-compatible path: a real
// ses_ id is pinned, so TranscriptPath skips the dir listing and exports it.
func TestOpenCodeTranscriptPathPinnedID(t *testing.T) {
	export, err := os.ReadFile(filepath.Join("testdata", "opencode", "export-session.json"))
	require.NoError(t, err)

	listCalls := 0
	var exported string
	restore := stubOcExec(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "session" {
			listCalls++
		}
		if args[0] == "export" {
			exported = args[1]
		}
		return export, nil
	})
	defer restore()

	p, ok := OpenCode{}.TranscriptPath("", "/work/agent-a", "ses_pinnedExactId123")
	require.True(t, ok)
	require.Equal(t, 0, listCalls, "a pinned ses_ id skips the dir listing")
	require.Equal(t, "ses_pinnedExactId123", exported)
	_ = os.Remove(p)
}

func TestOpenCodeTranscriptPathDegrades(t *testing.T) {
	// No workdir and no pinned id ⇒ nothing to resolve.
	_, ok := OpenCode{}.TranscriptPath("", "", "")
	require.False(t, ok)

	// Dir has no matching session ⇒ ok=false.
	list, err := os.ReadFile(filepath.Join("testdata", "opencode", "session-list.json"))
	require.NoError(t, err)
	restore := stubOcExec(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "session" {
			return list, nil
		}
		return nil, nil
	})
	defer restore()
	_, ok = OpenCode{}.TranscriptPath("", "/work/no-session-here", "")
	require.False(t, ok)
}

// stubOcExec swaps the package ocExec runner for the duration of a test.
func stubOcExec(fn func(context.Context, string, ...string) ([]byte, error)) func() {
	prev := ocExec
	ocExec = fn
	return func() { ocExec = prev }
}

// --- Transcript parsing -----------------------------------------------------

// TestOpenCodeParseTranscript parses the real captured `opencode export` fixture
// and asserts the neutral Turns warden's digest depends on.
func TestOpenCodeParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opencode", "export-session.json"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := OpenCode{}.ParseTranscript(f)
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
	require.Equal(t, 1, users)
	require.Equal(t, 1, assistants)

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "implement the add function")
	require.False(t, turns[0].Timestamp.IsZero(), "message created timestamp applied")

	// The assistant turn carries the model body and the patched file (from the
	// "patch" part — this weak model emitted the tool call as fenced text).
	require.Equal(t, "assistant", turns[1].Role)
	require.Contains(t, turns[1].Text, "\"name\": \"write\"")
	require.Equal(t, "edit", turns[1].ToolName, "a patch part implies an edit")
	require.NotEmpty(t, turns[1].Files, "patch part contributes edited files")
}

// TestOpenCodeParseTranscriptTool covers a real "tool" part (hand-authored from
// the schema, since the 3B fixture model never emitted one): the tool name and the
// file from its input must surface on the assistant turn.
func TestOpenCodeParseTranscriptTool(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "opencode", "export-tool.json"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := OpenCode{}.ParseTranscript(f)
	require.NoError(t, err)
	require.Len(t, turns, 2)

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "add a subtract function")

	a := turns[1]
	require.Equal(t, "assistant", a.Role)
	require.Equal(t, "write", a.ToolName, "explicit tool name wins over the patch fallback")
	require.Equal(t, []string{"calc.py"}, a.Files, "deduped across the write tool input and the patch part")
	require.Contains(t, a.Text, "subtract function")
}

func TestOpenCodeParseTranscriptBadJSON(t *testing.T) {
	_, err := OpenCode{}.ParseTranscript(strings.NewReader("not json"))
	require.Error(t, err)
}

// --- State / approval (degraded) --------------------------------------------

func TestOpenCodeStateDegrades(t *testing.T) {
	require.Equal(t, agentbackend.StateUnknown, OpenCode{}.DetectState("any pane content"))
	_, ok := OpenCode{}.ParseApproval("Do you want to proceed?")
	require.False(t, ok, "interactive approval parsing is deferred — degrade, not mis-parse")
}

// --- Capabilities / pricing -------------------------------------------------

func TestOpenCodeCapabilities(t *testing.T) {
	c := OpenCode{}.Capabilities()
	require.True(t, c.Resume, "OpenCode resumes (unlike Aider)")
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.True(t, c.StructuredTranscript, "Tier A: export JSON parses into Turns")
	require.False(t, c.SessionIDControl, "OpenCode mints its own ses_ id")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"default", "dangerously-skip-permissions"}, c.PermissionModes)
}

func TestOpenCodeNoPricing(t *testing.T) {
	_, ok := OpenCode{}.Pricing()
	require.False(t, ok, "BYO multi-provider ⇒ no warden-side pricing table yet")
}

func TestOpenCodeSystemPromptUnsupported(t *testing.T) {
	_, ok := OpenCode{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

func TestOpenCodeRegistered(t *testing.T) {
	b, err := agentbackend.Get("opencode")
	require.NoError(t, err)
	require.Equal(t, "OpenCode", b.DisplayName())
	require.Equal(t, "opencode", b.Binary())
}
