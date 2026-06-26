package repl

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakePrefiller returns canned suggestions, standing in for the local model.
type fakePrefiller struct{ vals map[string]string }

func (f fakePrefiller) Prefill(context.Context, string, string, []fieldSpec) map[string]string {
	return f.vals
}

// formSession wires a session whose gate is a real *Gate over a scripted reader,
// so RunCommand's form path (canForm() == true) actually runs.
func formSession(script string, fd *fakeDaemon) (*Session, *Gate) {
	g := NewGate(strings.NewReader(script), io.Discard)
	s := NewSession(&scriptChatter{}, fd, NewRegistry(), g, nil)
	return s, g
}

// --- gate-level form behaviour ---

func TestGateForm_EnumPickedByNumber(t *testing.T) {
	g := editGate("2\n") // model menu → option 2 (opus)
	fields := g.formFields("spawn_agent", "model")
	done := g.Form(context.Background(), "", ToolCall{Name: "spawn_agent", Args: map[string]any{}}, fields)
	require.Equal(t, "opus", done.Args["model"])
}

func TestGateForm_PrefillAcceptedWithEnter(t *testing.T) {
	g := editGate("\n") // Enter accepts the suggestion
	g.usePrefiller(fakePrefiller{vals: map[string]string{"type": "pr-review"}})
	fields := g.formFields("spawn_agent", "type")
	done := g.Form(context.Background(), "review auth", ToolCall{Name: "spawn_agent", Args: map[string]any{}}, fields)
	require.Equal(t, "pr-review", done.Args["type"], "Enter accepts the model's suggestion")
}

func TestGateForm_PrefillOverridden(t *testing.T) {
	g := editGate("2\n") // operator overrides the suggestion with option 2 (analysis)
	g.usePrefiller(fakePrefiller{vals: map[string]string{"type": "pr-review"}})
	fields := g.formFields("spawn_agent", "type")
	done := g.Form(context.Background(), "review auth", ToolCall{Name: "spawn_agent", Args: map[string]any{}}, fields)
	require.Equal(t, "analysis", done.Args["type"], "an explicit pick overrides the suggestion")
}

func TestGateForm_DashClearsField(t *testing.T) {
	g := editGate("-\n") // "-" clears back to the config default
	fields := g.formFields("spawn_agent", "model")
	done := g.Form(context.Background(), "", ToolCall{Name: "spawn_agent", Args: map[string]any{"model": "opus"}}, fields)
	_, ok := done.Args["model"]
	require.False(t, ok, "the field is cleared so config fills it")
}

func TestGateForm_InvalidEnumKeepsCurrent(t *testing.T) {
	g := editGate("gpt-4\n") // not a listed model → keep current
	fields := g.formFields("spawn_agent", "model")
	done := g.Form(context.Background(), "", ToolCall{Name: "spawn_agent", Args: map[string]any{"model": "sonnet"}}, fields)
	require.Equal(t, "sonnet", done.Args["model"], "an out-of-range entry never overwrites the value")
}

// --- RunCommand triggers ---

// /spawn with no prompt auto-opens the form (formAuto) and collects the required
// prompt, then the assembled call still passes the confirm gate.
func TestRunCommand_SpawnMissingPromptOpensForm(t *testing.T) {
	fd := &fakeDaemon{}
	s, _ := formSession("review the auth package\na\n", fd) // prompt answer, then approve
	out, handled := s.RunCommand(context.Background(), "/spawn")
	require.True(t, handled)
	require.NotContains(t, out, "usage:", "the form opened instead of printing usage")
	require.Equal(t, 1, fd.spawnCalls)
	require.Equal(t, "review the auth package", fd.lastSpawn.Prompt)
}

// `/spawn+` opens the full form even with a prompt already given, letting the
// operator fill an optional field (name) the quick path wouldn't ask for.
func TestRunCommand_SpawnPlusOpensFullForm(t *testing.T) {
	fd := &fakeDaemon{}
	// fields in order: prompt(keep), name("api"), type, repo, branch, worktree,
	// in_repo, model, permission_mode — then approve at the gate.
	s, _ := formSession("\napi\n\n\n\n\n\n\n\na\n", fd)
	s.RunCommand(context.Background(), "/spawn+ build the api")
	require.Equal(t, 1, fd.spawnCalls)
	require.Equal(t, "build the api", fd.lastSpawn.Prompt, "typed prompt is preserved")
	require.Equal(t, "api", fd.lastSpawn.Name, "optional field filled via the form")
}

// When the gate is non-interactive (the scripted spyGate in most tests), the
// form can't run, so /spawn with no prompt falls back to the usage line.
func TestRunCommand_NoFormWhenGateNotInteractive(t *testing.T) {
	fd := &fakeDaemon{}
	s := newTestSession(&scriptChatter{}, fd, alwaysApprove())
	out, _ := s.RunCommand(context.Background(), "/spawn")
	require.Contains(t, out, "usage:")
	require.Zero(t, fd.spawnCalls)
}
