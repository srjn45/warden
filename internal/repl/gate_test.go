package repl

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

// editGate builds a gate wired to the real registry, so the field-by-field
// editor prompts the tool's declared fields (not just the keys already present).
func editGate(script string) *Gate {
	g := NewGate(strings.NewReader(script), io.Discard)
	g.useRegistry(NewRegistry())
	return g
}

func TestGate_EditYieldsRevisedArgs(t *testing.T) {
	// 'e' to edit; the only field present is "type" (no registry), answered "docs".
	g := NewGate(strings.NewReader("e\ndocs\n"), io.Discard)
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development"}}})
	require.Equal(t, Edit, d.Action)
	require.Equal(t, "docs", d.Calls[0].Args["type"])
}

func TestGate_EditBlankKeepsOriginal(t *testing.T) {
	// 'e' to edit, then Enter (blank) keeps the proposed value.
	g := NewGate(strings.NewReader("e\n\n"), io.Discard)
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"type": "development"}}})
	require.Equal(t, Edit, d.Action)
	require.Equal(t, "development", d.Calls[0].Args["type"])
}

// TestGate_EditAddsOmittedField proves the schema-driven editor: the model
// proposed only a prompt, and the operator fills in a field (branch) the model
// left out. spawn_agent's fields are prompted in preferred order (prompt, name,
// type, repo, branch, …), so the script keeps prompt/name/type/repo, then sets
// branch, then EOFs to keep the rest.
func TestGate_EditAddsOmittedField(t *testing.T) {
	g := editGate("e\n\n\n\n\nfeature-x\n")
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"prompt": "review the docs"}}})
	require.Equal(t, Edit, d.Action)
	require.Equal(t, "review the docs", d.Calls[0].Args["prompt"], "untouched field kept")
	require.Equal(t, "feature-x", d.Calls[0].Args["branch"], "omitted field filled in")
}

// TestGate_EditShowsConfigDefault proves a blank field warden will fill from
// config renders its default in the bracket ("[default: …]") instead of "[]", so
// the operator can see what an empty answer resolves to.
func TestGate_EditShowsConfigDefault(t *testing.T) {
	var out bytes.Buffer
	// 'e' to edit, then Enter through each field (prompt, name, type, repo,
	// branch, worktree, in_repo, model) so the model/permission_mode prompts get
	// rendered before EOF.
	g := NewGate(strings.NewReader("e\n\n\n\n\n\n\n\n\n"), &out)
	g.useRegistry(NewRegistry())
	g.UseDefaults(map[string]string{"model": "opus", "permission_mode": "bypassPermissions"})
	g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"prompt": "review"}}})
	require.Contains(t, out.String(), "model [default: opus]:")
	require.Contains(t, out.String(), "permission_mode [default: bypassPermissions]:")
	// A field with no default and no value still shows the empty bracket.
	require.Contains(t, out.String(), "name []:")
}

// TestGate_EditCoercesBool proves a boolean field is parsed from y/n, not stored
// as the literal string. worktree is the 6th spawn_agent field (after prompt,
// name, type, repo, branch).
func TestGate_EditCoercesBool(t *testing.T) {
	g := editGate("e\n\n\n\n\n\ny\n")
	d := g.Confirm([]ToolCall{{Name: "spawn_agent", Args: map[string]any{"prompt": "x"}}})
	require.Equal(t, Edit, d.Action)
	require.Equal(t, true, d.Calls[0].Args["worktree"])
}
