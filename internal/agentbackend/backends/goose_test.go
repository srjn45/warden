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

func TestGooseLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "name is pinned as the session handle",
			opts: agentbackend.LaunchOpts{Name: "JIRA-1", Model: "qwen2.5-coder:3b", Mode: "default"},
			want: "goose session --name 'JIRA-1'",
		},
		{
			name: "model is ignored (session has no --model; config/env-driven)",
			opts: agentbackend.LaunchOpts{Name: "a1", Model: "llama3.2:3b", Mode: "auto"},
			want: "goose session --name 'a1'",
		},
		{
			name: "session id is ignored (Goose mints its own; warden pins --name)",
			opts: agentbackend.LaunchOpts{SessionID: "20260628_1", Name: "a1", Mode: "default"},
			want: "goose session --name 'a1'",
		},
		{
			name: "empty name omits --name (bare interactive launch)",
			opts: agentbackend.LaunchOpts{Mode: "default"},
			want: "goose session",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Goose{}.LaunchCmd(tt.opts))
		})
	}
}

// TestGooseLaunchQuotesName is the command-injection guard: a name with shell
// metacharacters must be single-quoted so it cannot break out of the line typed
// into a tmux pane.
func TestGooseLaunchQuotesName(t *testing.T) {
	got := Goose{}.LaunchCmd(agentbackend.LaunchOpts{Name: "a; touch /tmp/pwned #"})
	require.Equal(t, "goose session --name 'a; touch /tmp/pwned #'", got)
}

func TestGooseResumeCmd(t *testing.T) {
	// A pinned name ⇒ name-deterministic resume (richer than OpenCode's -c).
	cmd, ok := Goose{}.ResumeCmd(agentbackend.ResumeOpts{Name: "a1", Model: "m"})
	require.True(t, ok, "Goose supports resume (Caps.Resume=true)")
	require.Equal(t, "goose session -r --name 'a1'", cmd)

	// No name ⇒ resume most-recent for the pane's directory.
	cmd, ok = Goose{}.ResumeCmd(agentbackend.ResumeOpts{})
	require.True(t, ok)
	require.Equal(t, "goose session -r", cmd)
}

// TestGooseLaunchPromptArgEmpty pins that `goose session` takes no launch-line
// prompt, so the seeding fragment is empty and LaunchCmd stays a valid standalone
// interactive launch — the prompt is typed in after launch via PromptSeeder.
func TestGooseLaunchPromptArgEmpty(t *testing.T) {
	require.Equal(t, "", Goose{}.LaunchPromptArg("/state/prompts/job-1"))
}

func TestGoosePromptSeeder(t *testing.T) {
	var ps agentbackend.PromptSeeder = Goose{}
	text, ok := ps.PromptText("list the apis")
	require.True(t, ok)
	require.Equal(t, "list the apis", text)
	_, ok = ps.PromptText("")
	require.False(t, ok)
	require.Equal(t, "goose is ready", ps.ReadyMarker())
}

func TestGooseHeadlessCmd(t *testing.T) {
	argv, ok := Goose{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"goose", "run", "--no-session", "--quiet", "-t", "classify this"}, argv)
}

// --- Session resolution (dir-scoped) ----------------------------------------

func TestNewestGooseSessionForDir(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("testdata", "goose", "session-list.json"))
	require.NoError(t, err)

	// Dir A has two sessions; the newest (by updated_at) wins.
	id, ok := newestGooseSessionForDir(list, "/work/agent-a")
	require.True(t, ok)
	require.Equal(t, "20260628_3", id)

	// Dir B has one session.
	id, ok = newestGooseSessionForDir(list, "/work/agent-b")
	require.True(t, ok)
	require.Equal(t, "20260628_2", id)

	// A directory with no session ⇒ not found (transcript degrades).
	_, ok = newestGooseSessionForDir(list, "/work/unknown")
	require.False(t, ok)

	// Garbage input ⇒ not found, no panic.
	_, ok = newestGooseSessionForDir([]byte("not json"), "/work/agent-a")
	require.False(t, ok)
}

// --- Transcript orchestration (subprocess stubbed) --------------------------

// TestGooseTranscriptPathDirScoped exercises the live path: TranscriptPath lists
// sessions for the workdir, picks the newest, exports it, and materializes it to
// a file the caller can open.
func TestGooseTranscriptPathDirScoped(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("testdata", "goose", "session-list.json"))
	require.NoError(t, err)
	export, err := os.ReadFile(filepath.Join("testdata", "goose", "export-session.json"))
	require.NoError(t, err)

	var listedDir, exportedID string
	restore := stubGooseExec(func(_ context.Context, dir string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "session" && args[1] == "list":
			listedDir = dir
			return list, nil
		case args[0] == "session" && args[1] == "export":
			// args: session export --session-id <id> --format json
			exportedID = args[3]
			return export, nil
		}
		return nil, nil
	})
	defer restore()

	p, ok := Goose{}.TranscriptPath("", "/work/agent-a", "ignored-claude-id")
	require.True(t, ok)
	require.Equal(t, "/work/agent-a", listedDir, "session list runs in the agent workdir")
	require.Equal(t, "20260628_3", exportedID, "exports the newest session for the dir")

	got, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, export, got, "materialized file holds the export JSON")
	_ = os.Remove(p)
}

func TestGooseTranscriptPathDegrades(t *testing.T) {
	// No workdir ⇒ nothing to resolve.
	_, ok := Goose{}.TranscriptPath("", "", "")
	require.False(t, ok)

	// Dir has no matching session ⇒ ok=false.
	list, err := os.ReadFile(filepath.Join("testdata", "goose", "session-list.json"))
	require.NoError(t, err)
	restore := stubGooseExec(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "session" && args[1] == "list" {
			return list, nil
		}
		return nil, nil
	})
	defer restore()
	_, ok = Goose{}.TranscriptPath("", "/work/no-session-here", "")
	require.False(t, ok)
}

// stubGooseExec swaps the package gooseExec runner for the duration of a test.
func stubGooseExec(fn func(context.Context, string, ...string) ([]byte, error)) func() {
	prev := gooseExec
	gooseExec = fn
	return func() { gooseExec = prev }
}

// --- Transcript parsing -----------------------------------------------------

// TestGooseParseTranscript parses the real captured `goose session export`
// fixture (an llama3.2:3b run that emitted a genuine toolRequest) and asserts
// the neutral Turns warden's digest depends on.
func TestGooseParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "goose", "export-session.json"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Goose{}.ParseTranscript(f)
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
	require.Equal(t, 1, users, "the toolResponse user message carries no prompt and is skipped")
	require.Equal(t, 2, assistants, "the toolRequest turn and the final text turn")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "Create greeting.txt")
	require.False(t, turns[0].Timestamp.IsZero(), "message created timestamp applied")

	// The assistant turn carries the tool name and the written file from the
	// toolRequest part's arguments.
	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "write", turns[1].ToolName)
	require.Equal(t, []string{"/tmp/greeting.txt"}, turns[1].Files)
}

// TestGooseParseTranscriptTextOnly covers the $0-local weak-model reality: the
// qwen2.5-coder:3b fixture emitted its tool call as fenced JSON *text* (not a
// real toolRequest), so the assistant turn has no tool name and the body keeps
// the fenced text verbatim — parsed faithfully, not mis-attributed.
func TestGooseParseTranscriptTextOnly(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "goose", "export-text.json"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Goose{}.ParseTranscript(f)
	require.NoError(t, err)
	require.Len(t, turns, 2)

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "Create a file calc.py")

	require.Equal(t, "assistant", turns[1].Role)
	require.Empty(t, turns[1].ToolName, "fenced tool-text is not a real toolRequest")
	require.Contains(t, turns[1].Text, `"name": "write"`)
}

func TestGooseParseTranscriptBadJSON(t *testing.T) {
	_, err := Goose{}.ParseTranscript(strings.NewReader("not json"))
	require.Error(t, err)
}

// --- State / approval (degraded) --------------------------------------------

func TestGooseStateDegrades(t *testing.T) {
	require.Equal(t, agentbackend.StateUnknown, Goose{}.DetectState("any pane content"))
	_, ok := Goose{}.ParseApproval("Do you want to proceed?")
	require.False(t, ok, "interactive approval parsing is deferred — degrade, not mis-parse")
}

// --- Capabilities / pricing -------------------------------------------------

func TestGooseCapabilities(t *testing.T) {
	c := Goose{}.Capabilities()
	require.True(t, c.Resume, "Goose resumes (name-deterministic)")
	require.True(t, c.Headless)
	require.True(t, c.StructuredTranscript, "Tier A: JSON export parses into Turns")
	require.False(t, c.ModelSelection, "interactive `goose session` takes no --model")
	require.False(t, c.SessionIDControl, "Goose mints its own date-stamped id")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"auto", "approve", "chat", "smart_approve"}, c.PermissionModes)
}

func TestGooseNoPricing(t *testing.T) {
	_, ok := Goose{}.Pricing()
	require.False(t, ok, "BYO multi-provider ⇒ no warden-side pricing table yet")
}

func TestGooseSystemPromptUnsupported(t *testing.T) {
	_, ok := Goose{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

func TestGooseRegistered(t *testing.T) {
	b, err := agentbackend.Get("goose")
	require.NoError(t, err)
	require.Equal(t, "Goose", b.DisplayName())
	require.Equal(t, "goose", b.Binary())
}
