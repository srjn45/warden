package backends

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// TestTerminalRegistered pins Terminal into the registry under "terminal" so it
// is an accepted --backend value and resolvable exactly as core resolves it.
func TestTerminalRegistered(t *testing.T) {
	b, err := agentbackend.Get("terminal")
	require.NoError(t, err)
	require.Equal(t, "terminal", b.ID())
	require.NotEmpty(t, b.DisplayName())
}

// TestTerminalLaunchCmd asserts the launch line opens the user's shell (falling
// back to bash) and ignores every AI-oriented LaunchOpts field.
func TestTerminalLaunchCmd(t *testing.T) {
	got := Terminal{}.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: "sid", Name: "a1", Model: "opus", Mode: "auto",
	})
	require.Equal(t, `${SHELL:-bash}`, got)
	require.True(t, strings.Contains(got, "SHELL"), "must launch the user's shell")
}

// TestTerminalDegrades locks in the fully-degraded contract: no resume, no
// headless, no prompt seeding, no transcript, no approval, no pricing.
func TestTerminalDegrades(t *testing.T) {
	term := Terminal{}

	if _, ok := term.ResumeCmd(agentbackend.ResumeOpts{}); ok {
		t.Fatal("terminal must not support resume")
	}
	require.Equal(t, "", term.LaunchPromptArg("/tmp/prompt"), "prompt is never seeded into a shell")
	if _, ok := term.HeadlessCmd("classify this"); ok {
		t.Fatal("terminal has no headless one-shot")
	}
	if _, ok := term.TranscriptPath("proj", "/work", "sid"); ok {
		t.Fatal("terminal has no transcript")
	}
	turns, err := term.ParseTranscript(strings.NewReader("anything"))
	require.NoError(t, err)
	require.Empty(t, turns)
	if _, ok := term.ParseApproval("y/n?"); ok {
		t.Fatal("terminal has no approval UI")
	}
	require.Equal(t, agentbackend.StateUnknown, term.DetectState("$ some pane output"))
	if _, ok := term.SystemPromptFlag("persona"); ok {
		t.Fatal("terminal has no system-prompt injection")
	}
	if _, ok := term.Pricing(); ok {
		t.Fatal("terminal has no pricing")
	}
}

// TestTerminalNotPromptSeeder guards the deliberate choice NOT to type the task
// prompt into the shell (it would execute as a command).
func TestTerminalNotPromptSeeder(t *testing.T) {
	if _, ok := agentbackend.Backend(Terminal{}).(agentbackend.PromptSeeder); ok {
		t.Fatal("terminal must not implement PromptSeeder — a shell would run the prompt")
	}
}

// TestTerminalCaps documents that every AI capability is off.
func TestTerminalCaps(t *testing.T) {
	c := Terminal{}.Capabilities()
	require.False(t, c.Resume)
	require.False(t, c.Headless)
	require.False(t, c.ModelSelection)
	require.False(t, c.StructuredTranscript)
	require.False(t, c.SystemPromptInject)
	require.False(t, c.SessionIDControl)
	require.Empty(t, c.PermissionModes)
}
