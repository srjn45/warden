package orchestrator

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/llm"
	"github.com/stretchr/testify/require"
)

func TestArgStrCoercions(t *testing.T) {
	args := map[string]any{
		"s":    "hello",
		"int":  float64(42), // JSON numbers decode to float64
		"frac": float64(3.5),
		"b":    true,
	}
	require.Equal(t, "hello", argStr(args, "s"))
	require.Equal(t, "42", argStr(args, "int"), "a whole JSON number renders without a .0")
	require.Equal(t, "3.5", argStr(args, "frac"))
	require.Equal(t, "true", argStr(args, "b"))
	require.Equal(t, "", argStr(args, "missing"), "an absent key is the empty string, not a panic")
}

func TestArgBoolCoercions(t *testing.T) {
	require.True(t, argBool(map[string]any{"x": true}, "x"))
	require.True(t, argBool(map[string]any{"x": "true"}, "x"))
	require.True(t, argBool(map[string]any{"x": "1"}, "x"))
	require.True(t, argBool(map[string]any{"x": "yes"}, "x"))
	require.False(t, argBool(map[string]any{"x": "no"}, "x"))
	require.False(t, argBool(map[string]any{"x": float64(1)}, "x"), "a number is not a bool")
	require.False(t, argBool(map[string]any{}, "x"), "an absent key is false")
}

func TestArgIntCoercions(t *testing.T) {
	require.Equal(t, 42, argInt(map[string]any{"n": float64(42)}, "n"))
	require.Equal(t, 7, argInt(map[string]any{"n": 7}, "n"))
	require.Equal(t, 13, argInt(map[string]any{"n": "13"}, "n"))
	require.Equal(t, 0, argInt(map[string]any{"n": "not-a-number"}, "n"))
	require.Equal(t, 0, argInt(map[string]any{}, "n"), "an absent key is zero")
}

func TestRequireStr(t *testing.T) {
	got, err := requireStr(map[string]any{"k": "  value  "}, "k")
	require.NoError(t, err)
	require.Equal(t, "value", got, "the value is trimmed")

	_, err = requireStr(map[string]any{"k": "   "}, "k")
	require.Error(t, err, "a blank value is treated as missing")

	_, err = requireStr(map[string]any{}, "k")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing required argument")
}

func TestJsonish(t *testing.T) {
	require.Equal(t, `{"a":1}`, jsonish(map[string]any{"a": 1}))
	require.Equal(t, `["x","y"]`, jsonish([]string{"x", "y"}))
}

func TestBuildEscalationPrompt(t *testing.T) {
	tools := []llm.ToolSchema{
		{Name: "spawn_agent", Description: "spawn a coding agent"},
		{Name: "list_agents", Description: "list active agents"},
	}
	got := buildEscalationPrompt("scale up the backend work", tools)

	// The system framing must forbid prose and demand a JSON array of tool calls.
	require.Contains(t, got, "ONLY a JSON array")
	require.Contains(t, got, "never write code")
	// Every tool is offered with its description.
	require.Contains(t, got, "- spawn_agent: spawn a coding agent")
	require.Contains(t, got, "- list_agents: list active agents")
	// The operator request is appended verbatim at the end.
	require.True(t, strings.HasSuffix(got, "Operator request: scale up the backend work"))
}

func TestBuildEscalationPromptNoTools(t *testing.T) {
	got := buildEscalationPrompt("do x", nil)
	require.NotContains(t, got, "Available tools:")
	require.Contains(t, got, "Operator request: do x")
}
