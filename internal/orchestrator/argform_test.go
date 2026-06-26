package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFieldOptions(t *testing.T) {
	require.Equal(t, []string{"sonnet", "opus", "haiku", "fable"}, fieldOptions("model"))
	require.Contains(t, fieldOptions("permission_mode"), "acceptEdits")
	require.Contains(t, fieldOptions("type"), "pr-review")
	require.Nil(t, fieldOptions("prompt"), "free-text field has no option set")
	require.Nil(t, fieldOptions("branch"))
}

func TestResolveOption(t *testing.T) {
	opts := fieldOptions("model") // sonnet, opus, haiku, fable
	for _, tc := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"2", "opus", true},        // by 1-based number
		{"opus", "opus", true},     // by value
		{"OPUS", "opus", true},     // case-insensitive
		{"sonnet", "sonnet", true}, // first option
		{"4", "fable", true},       // last option
		{"5", "", false},           // number out of range
		{"0", "", false},           // number below range
		{"gpt-4", "", false},       // not a listed value
		{"", "", false},            // blank
	} {
		got, ok := resolveOption(tc.in, opts)
		require.Equal(t, tc.wantOK, ok, "input %q", tc.in)
		require.Equal(t, tc.want, got, "input %q", tc.in)
	}
}

func TestRenderOptions_MarksSelected(t *testing.T) {
	out := renderOptions(fieldOptions("model"), "opus")
	require.Contains(t, out, "1) sonnet")
	require.Contains(t, out, "[2) opus ←]", "the pre-selection is marked")
	require.NotContains(t, out, "[1) sonnet", "only the selected one is bracketed")
}

func TestParsePrefill_KeepsValidDropsInvalid(t *testing.T) {
	fields := []fieldSpec{
		{name: "type", kind: "string"},
		{name: "name", kind: "string"},
		{name: "model", kind: "string"},
	}
	// type valid enum, name free text, model out-of-range (dropped), and an
	// unknown field (dropped).
	raw := `{"type":"pr-review","name":"auth-review","model":"gpt-4","repo":"/x"}`
	got := parsePrefill(raw, fields)
	require.Equal(t, "pr-review", got["type"])
	require.Equal(t, "auth-review", got["name"])
	require.NotContains(t, got, "model", "out-of-range enum suggestion is dropped")
	require.NotContains(t, got, "repo", "unknown field is dropped")
}

func TestParsePrefill_NormalisesEnumValue(t *testing.T) {
	fields := []fieldSpec{{name: "model", kind: "string"}}
	require.Equal(t, "opus", parsePrefill(`{"model":"OPUS"}`, fields)["model"])
	require.Equal(t, "opus", parsePrefill(`{"model":"2"}`, fields)["model"], "a number resolves to the value")
}

func TestParsePrefill_ToleratesChattyPrefixAndBadJSON(t *testing.T) {
	fields := []fieldSpec{{name: "name", kind: "string"}}
	require.Equal(t, "x", parsePrefill(`Sure! {"name":"x"}`, fields)["name"])
	require.Nil(t, parsePrefill("not json at all", fields))
	require.Nil(t, parsePrefill("{}", fields), "empty object yields no suggestions")
}

// fakeCompleter is a scripted llm.Completer for the prefiller.
type fakeCompleter struct {
	out string
	err error
}

func (f fakeCompleter) Complete(context.Context, string) (string, error) { return f.out, f.err }

func TestLLMPrefiller_SuggestsAndValidates(t *testing.T) {
	fields := []fieldSpec{{name: "type", kind: "string"}, {name: "name", kind: "string"}}
	p := llmPrefiller{comp: fakeCompleter{out: `{"type":"analysis","name":"sec"}`}}
	got := p.Prefill(context.Background(), "spawn_agent", "review security", fields)
	require.Equal(t, "analysis", got["type"])
	require.Equal(t, "sec", got["name"])
}

func TestLLMPrefiller_DegradesGracefully(t *testing.T) {
	fields := []fieldSpec{{name: "name", kind: "string"}}
	// nil completer, empty query, and a model error all yield no suggestions
	// (the form falls back to a plain, model-free pick-list).
	require.Nil(t, llmPrefiller{comp: nil}.Prefill(context.Background(), "t", "q", fields))
	require.Nil(t, llmPrefiller{comp: fakeCompleter{out: "{}"}}.Prefill(context.Background(), "t", "", fields))
	require.Nil(t, llmPrefiller{comp: fakeCompleter{err: context.DeadlineExceeded}}.Prefill(context.Background(), "t", "q", fields))
}
