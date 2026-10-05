package fastbrain

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func prResp(status Status, raw string) Response {
	r := Response{Status: status}
	if status == StatusOK {
		r.Output.Parsed = json.RawMessage(raw)
	}
	return r
}

func TestParsePRSummary(t *testing.T) {
	long := strings.Repeat("a", 90)
	cases := []struct {
		name        string
		r           Response
		title, body string
	}{
		{"ok", prResp(StatusOK, `{"title":"feat: add x","body":"## What changed\n- x"}`), "feat: add x", "## What changed\n- x"},
		{"trims", prResp(StatusOK, `{"title":"  fix: y  ","body":"  b  "}`), "fix: y", "b"},
		{"multiline title keeps first line", prResp(StatusOK, `{"title":"feat: a\nmore","body":"b"}`), "feat: a", "b"},
		{"long title capped", prResp(StatusOK, `{"title":"`+long+`","body":"b"}`), strings.Repeat("a", 71) + "…", "b"},
		{"missing body", prResp(StatusOK, `{"title":"feat: a"}`), "feat: a", ""},
		{"missing title", prResp(StatusOK, `{"body":"b"}`), "", "b"},
		{"wrong types", prResp(StatusOK, `{"title":3,"body":[]}`), "", ""},
		{"empty object", prResp(StatusOK, `{}`), "", ""},
		{"timeout", prResp(StatusTimeout, ""), "", ""},
		{"invalid json", prResp(StatusInvalidJSON, ""), "", ""},
		{"no runner", prResp(StatusNoRunner, ""), "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, body := ParsePRSummary(c.r)
			require.Equal(t, c.title, title)
			require.Equal(t, c.body, body)
		})
	}
}

func TestPRSummaryPromptCapsInput(t *testing.T) {
	p := PRSummaryPrompt("task", strings.Repeat("s", 50000), strings.Repeat("c", 50000))
	require.Less(t, len(p), 10000)
	require.Contains(t, p, "Task:\ntask")
	require.Contains(t, p, "Diff stat:")
	require.Contains(t, p, "Commits:")
}
