package fastbrain

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/llm"
	"github.com/stretchr/testify/require"
)

var testTools = []llm.ToolSchema{{Name: "ls", Description: "list", Parameters: map[string]any{"properties": map[string]any{"path": 1}}}}

func chat(t *testing.T, r Runner) llm.Reply {
	t.Helper()
	rep, err := NewFastBrainChatter(NewEngine(r, nil)).Chat(context.Background(),
		[]llm.Message{{Role: llm.RoleUser, Content: "hi"}}, testTools)
	require.NoError(t, err)
	return rep
}

func TestChatterToolCalls(t *testing.T) {
	rep := chat(t, fixed("```json\n{\"text\":\"\",\"tool_calls\":[{\"name\":\"ls\",\"args\":{\"path\":\".\"}},{\"name\":\"\"}]}\n```"))
	require.Equal(t, []llm.ToolCall{{Name: "ls", Args: map[string]any{"path": "."}}}, rep.ToolCalls)
}

func TestChatterProse(t *testing.T) {
	rep := chat(t, fixed(`{"text":" hello "}`))
	require.Equal(t, "hello", rep.Text)
	require.Empty(t, rep.ToolCalls)
}

func TestChatterFailOpen(t *testing.T) {
	for name, r := range map[string]Runner{
		"invalid": fixed("not json"),
		"error":   RunnerFunc(func(context.Context, string) (string, error) { return "", errors.New("x") }),
		"timeout": blocking(),
	} {
		t.Run(name, func(t *testing.T) { require.Equal(t, llm.Reply{}, chat(t, r)) })
	}
	rep, err := NewFastBrainChatter(NewEngine(nil, nil)).Chat(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Equal(t, llm.Reply{}, rep)
}

func TestChatterNilEngineAndCanceled(t *testing.T) {
	_, err := NewFastBrainChatter(nil).Chat(context.Background(), nil, nil)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewFastBrainChatter(NewEngine(fixed(`{}`), nil)).Chat(ctx, nil, nil)
	require.Error(t, err)
}

func TestReplPromptContents(t *testing.T) {
	p := replPrompt([]llm.Message{{Role: llm.RoleUser, Content: "q"}, {Role: llm.RoleTool, ToolName: "ls", Content: "out"}}, testTools)
	require.Contains(t, p, "- ls: list (args: path)")
	require.Contains(t, p, "[user] q")
	require.Contains(t, p, "[tool ls result] out")
}
