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

func TestCrushLaunchCmd(t *testing.T) {
	tests := []struct {
		name string
		opts agentbackend.LaunchOpts
		want string
	}{
		{
			name: "default (prompt) mode launches the bare TUI",
			opts: agentbackend.LaunchOpts{Model: "ollama/qwen2.5-coder:3b", Mode: "default"},
			want: "crush", // no -m: the TUI takes no model flag (config-driven)
		},
		{
			name: "yolo mode adds the flag",
			opts: agentbackend.LaunchOpts{Mode: "yolo"},
			want: "crush --yolo",
		},
		{
			name: "claude 'auto' folds onto --yolo",
			opts: agentbackend.LaunchOpts{Mode: "auto"},
			want: "crush --yolo",
		},
		{
			name: "bypassPermissions folds onto --yolo",
			opts: agentbackend.LaunchOpts{Mode: "bypassPermissions"},
			want: "crush --yolo",
		},
		{
			name: "session id, name and model are all ignored on the TUI launch",
			opts: agentbackend.LaunchOpts{SessionID: "9c76cb01dc3e5252", Name: "JIRA-1", Model: "m", Mode: "default"},
			want: "crush",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Crush{}.LaunchCmd(tt.opts))
		})
	}
}

func TestCrushResumeCmd(t *testing.T) {
	// No real crush id pinned (warden's placeholder UUID) ⇒ dir-scoped --continue.
	cmd, ok := Crush{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "550e8400-e29b-41d4-a716-446655440000"})
	require.True(t, ok, "Crush supports resume (Caps.Resume=true)")
	require.Equal(t, "crush --continue", cmd)

	// A real 16-hex id pinned (future discover-then-pin) ⇒ exact-id --session.
	cmd, ok = Crush{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "9c76cb01dc3e5252"})
	require.True(t, ok)
	require.Equal(t, "crush --session '9c76cb01dc3e5252'", cmd)

	// yolo mode carries onto resume.
	cmd, ok = Crush{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "x", Mode: "yolo"})
	require.True(t, ok)
	require.Equal(t, "crush --continue --yolo", cmd)
}

// TestCrushResumeQuotesSessionID is the command-injection guard for a pinned id
// (defense in depth — a real id is 16 hex, but the value is still shell-quoted).
// A non-matching id falls through to --continue, so we assert a valid-shaped id
// is single-quoted.
func TestCrushResumeQuotesSessionID(t *testing.T) {
	cmd, ok := Crush{}.ResumeCmd(agentbackend.ResumeOpts{SessionID: "abcdef0123456789"})
	require.True(t, ok)
	require.Equal(t, "crush --session 'abcdef0123456789'", cmd)
}

// TestCrushLaunchPromptArg documents the seeding gap: the interactive TUI takes no
// initial prompt, so the launch fragment is always empty.
func TestCrushLaunchPromptArg(t *testing.T) {
	require.Equal(t, "", Crush{}.LaunchPromptArg("/state/prompts/job-1"))
}

func TestCrushHeadlessCmd(t *testing.T) {
	argv, ok := Crush{}.HeadlessCmd("classify this")
	require.True(t, ok)
	require.Equal(t, []string{"crush", "run", "--quiet", "classify this"}, argv)
}

func TestLooksLikeCrushSessionID(t *testing.T) {
	require.True(t, looksLikeCrushSessionID("9c76cb01dc3e5252"))
	require.True(t, looksLikeCrushSessionID("abcdef0123456789"))
	require.False(t, looksLikeCrushSessionID("9C76CB01DC3E5252"), "uppercase hex is not the minted form")
	require.False(t, looksLikeCrushSessionID("9c76cb01dc3e525"), "15 chars is too short")
	require.False(t, looksLikeCrushSessionID("550e8400-e29b-41d4-a716-446655440000"), "warden UUID is not a crush id")
	require.False(t, looksLikeCrushSessionID(""))
}

// --- Session resolution (dir-scoped) ----------------------------------------

func TestNewestCrushSession(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("testdata", "crush", "session-list.json"))
	require.NoError(t, err)

	id, ok := newestCrushSession(list)
	require.True(t, ok)
	// The list is cwd-scoped by Crush; the newest by `modified` wins.
	require.Equal(t, "480c291bd53e1425", id)

	// Empty list ⇒ not found (transcript degrades).
	_, ok = newestCrushSession([]byte("[]"))
	require.False(t, ok)

	// Garbage input ⇒ not found, no panic.
	_, ok = newestCrushSession([]byte("not json"))
	require.False(t, ok)
}

// --- Transcript orchestration (subprocess stubbed) --------------------------

// TestCrushTranscriptPathDirScoped exercises the live path: no real crush id
// pinned, so TranscriptPath lists sessions (cwd-scoped), shows the newest, and
// materializes it to a file the caller can open.
func TestCrushTranscriptPathDirScoped(t *testing.T) {
	list, err := os.ReadFile(filepath.Join("testdata", "crush", "session-list.json"))
	require.NoError(t, err)
	show, err := os.ReadFile(filepath.Join("testdata", "crush", "show-480c291bd53e1425.json"))
	require.NoError(t, err)

	var listed, shownID string
	restore := stubCrushExec(func(_ context.Context, dir string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "session" && args[1] == "list":
			listed = dir
			return list, nil
		case args[0] == "session" && args[1] == "show":
			shownID = args[2]
			return show, nil
		}
		return nil, nil
	})
	defer restore()

	p, ok := Crush{}.TranscriptPath("", "/work/agent-a", "550e8400-uuid-placeholder")
	require.True(t, ok)
	require.Equal(t, "/work/agent-a", listed, "session list runs in the agent workdir")
	require.Equal(t, "480c291bd53e1425", shownID, "shows the newest session for the dir")

	got, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, show, got, "materialized file holds the session-show JSON")
	_ = os.Remove(p)
}

// TestCrushTranscriptPathPinnedID exercises the forward-compatible path: a real
// 16-hex id is pinned, so TranscriptPath skips the dir listing and shows it.
func TestCrushTranscriptPathPinnedID(t *testing.T) {
	show, err := os.ReadFile(filepath.Join("testdata", "crush", "show-480c291bd53e1425.json"))
	require.NoError(t, err)

	listCalls := 0
	var shownID string
	restore := stubCrushExec(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "session" && args[1] == "list" {
			listCalls++
		}
		if args[0] == "session" && args[1] == "show" {
			shownID = args[2]
		}
		return show, nil
	})
	defer restore()

	p, ok := Crush{}.TranscriptPath("", "/work/agent-a", "480c291bd53e1425")
	require.True(t, ok)
	require.Equal(t, 0, listCalls, "a pinned crush id skips the dir listing")
	require.Equal(t, "480c291bd53e1425", shownID)
	_ = os.Remove(p)
}

func TestCrushTranscriptPathDegrades(t *testing.T) {
	// No workdir and no pinned id ⇒ nothing to resolve.
	_, ok := Crush{}.TranscriptPath("", "", "")
	require.False(t, ok)

	// Dir has no sessions ⇒ ok=false.
	restore := stubCrushExec(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "session" && args[1] == "list" {
			return []byte("[]"), nil
		}
		return nil, nil
	})
	defer restore()
	_, ok = Crush{}.TranscriptPath("", "/work/no-session-here", "")
	require.False(t, ok)
}

// stubCrushExec swaps the package crushExec runner for the duration of a test.
func stubCrushExec(fn func(context.Context, string, ...string) ([]byte, error)) func() {
	prev := crushExec
	crushExec = fn
	return func() { crushExec = prev }
}

// --- Transcript parsing -----------------------------------------------------

// TestCrushParseTranscript parses the real captured `crush session show --json`
// fixture (a view tool_call from the 3B rig) and asserts the neutral Turns.
func TestCrushParseTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "crush", "show-480c291bd53e1425.json"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Crush{}.ParseTranscript(f)
	require.NoError(t, err)

	var users, assistants, tools int
	for _, tr := range turns {
		switch tr.Role {
		case "user":
			users++
		case "assistant":
			assistants++
		case "tool":
			tools++
		}
	}
	require.Equal(t, 1, users)
	require.Equal(t, 2, assistants, "tool_call turn + final text turn")
	require.Equal(t, 0, tools, "standalone tool_result messages are not emitted as Turns")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "Read the file README.md")
	require.False(t, turns[0].Timestamp.IsZero(), "message created timestamp applied")

	// The first assistant turn is the tool_call: name + extracted file path.
	require.Equal(t, "assistant", turns[1].Role)
	require.Equal(t, "view", turns[1].ToolName)
	require.Equal(t, []string{"README.md"}, turns[1].Files, "file_path extracted from the JSON-string input")
}

// TestCrushParseTranscriptEdits covers multiple tool_call parts in one assistant
// turn (write + edit): the first tool name wins and every file is deduped/collected.
func TestCrushParseTranscriptEdits(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "crush", "show-synthetic-edits.json"))
	require.NoError(t, err)
	defer f.Close()

	turns, err := Crush{}.ParseTranscript(f)
	require.NoError(t, err)
	require.Len(t, turns, 3, "user + 2 assistant (the tool_result message is not a Turn)")

	require.Equal(t, "user", turns[0].Role)
	require.Contains(t, turns[0].Text, "Refactor parser.go")

	a := turns[1]
	require.Equal(t, "assistant", a.Role)
	require.Equal(t, "edit", a.ToolName, "first tool_call name wins")
	require.Equal(t, []string{"parser.go", "parser_test.go"}, a.Files, "files collected from both tool_call inputs")
	require.Contains(t, a.Text, "refactor the parser", "reasoning parts are excluded; only text surfaces")

	require.Equal(t, "assistant", turns[2].Role)
	require.Equal(t, "Done.", turns[2].Text)
}

func TestCrushParseTranscriptBadJSON(t *testing.T) {
	_, err := Crush{}.ParseTranscript(strings.NewReader("not json"))
	require.Error(t, err)
}

// --- State / approval (degraded) --------------------------------------------

func TestCrushStateDegrades(t *testing.T) {
	require.Equal(t, agentbackend.StateUnknown, Crush{}.DetectState("any pane content"))
	_, ok := Crush{}.ParseApproval("Do you want to proceed?")
	require.False(t, ok, "interactive TUI approval parsing is deferred — degrade, not mis-parse")
}

// --- Capabilities / pricing -------------------------------------------------

func TestCrushCapabilities(t *testing.T) {
	c := Crush{}.Capabilities()
	require.True(t, c.Resume, "Crush resumes (--continue / --session)")
	require.True(t, c.Headless)
	require.True(t, c.ModelSelection)
	require.True(t, c.StructuredTranscript, "Tier A: session-show JSON parses into Turns")
	require.False(t, c.SessionIDControl, "Crush mints its own 16-hex id")
	require.False(t, c.SystemPromptInject)
	require.Equal(t, []string{"default", "yolo"}, c.PermissionModes)
}

func TestCrushNoPricing(t *testing.T) {
	_, ok := Crush{}.Pricing()
	require.False(t, ok, "BYO multi-provider ⇒ no warden-side pricing table yet")
}

func TestCrushSystemPromptUnsupported(t *testing.T) {
	_, ok := Crush{}.SystemPromptFlag("hint")
	require.False(t, ok)
}

func TestCrushRegistered(t *testing.T) {
	b, err := agentbackend.Get("crush")
	require.NoError(t, err)
	require.Equal(t, "Crush", b.DisplayName())
	require.Equal(t, "crush", b.Binary())
}
