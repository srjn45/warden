package orchestrator

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGate_ApproveRunsAll(t *testing.T) {
	g := NewGate(strings.NewReader("a\n"), io.Discard)
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development"}}})
	require.Equal(t, Approve, d.Action)
	require.Len(t, d.Calls, 1)
}

func TestGate_RejectByDefaultOnEOF(t *testing.T) {
	g := NewGate(strings.NewReader(""), io.Discard)
	require.Equal(t, Reject, g.Confirm([]ToolCall{{Name: "commit"}}).Action)
}

func TestGate_RejectOnUnrecognizedKey(t *testing.T) {
	g := NewGate(strings.NewReader("maybe\n"), io.Discard)
	require.Equal(t, Reject, g.Confirm([]ToolCall{{Name: "commit"}}).Action)
}

func TestGate_RendersEveryCallInBatch(t *testing.T) {
	var out bytes.Buffer
	NewGate(strings.NewReader("r\n"), &out).Confirm([]ToolCall{
		{Name: "spawn_agent", Args: map[string]any{"prompt": "a"}},
		{Name: "spawn_agent", Args: map[string]any{"prompt": "b"}}})
	require.Equal(t, 2, strings.Count(out.String(), "spawn_agent"))
}

func TestGate_EditYieldsRevisedArgs(t *testing.T) {
	g := NewGate(strings.NewReader("e\n{\"type\":\"docs\"}\n"), io.Discard)
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development"}}})
	require.Equal(t, Edit, d.Action)
	require.Equal(t, "docs", d.Calls[0].Args["type"])
}

func TestGate_EditBlankKeepsOriginal(t *testing.T) {
	g := NewGate(strings.NewReader("e\n\n"), io.Discard)
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development"}}})
	require.Equal(t, Edit, d.Action)
	require.Equal(t, "development", d.Calls[0].Args["type"])
}
