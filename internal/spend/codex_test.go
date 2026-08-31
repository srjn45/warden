package spend

import (
	"strings"
	"testing"
)

func TestCodexParseUsageUsesLastCumulativeSnapshot(t *testing.T) {
	jsonl := `not json
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":5,"output_tokens":2,"reasoning_output_tokens":1}}}}
{"type":"event_msg","payload":{"type":"agent_message"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"cached_input_tokens":10,"output_tokens":4,"reasoning_output_tokens":2}}}}
{ broken`

	u, ok := GetParser("codex").ParseUsage(strings.NewReader(jsonl))
	if !ok || u.InputTokens != 40 || u.OutputTokens != 6 || u.Total() != 46 {
		t.Fatalf("got=%+v ok=%v, want input=40 output=6 total=46 true", u, ok)
	}
}

func TestCodexParseUsageNoUsage(t *testing.T) {
	jsonl := `{"type":"event_msg","payload":{"type":"token_count"}}
{"type":"event_msg","payload":{"type":"agent_message"}}`
	if u, ok := GetParser("codex").ParseUsage(strings.NewReader(jsonl)); ok || u.Total() != 0 {
		t.Fatalf("got=%+v ok=%v, want zero/false", u, ok)
	}
}

func TestSpendParserSelectionPreservesClaudeFallback(t *testing.T) {
	codex := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":8,"cached_input_tokens":2,"output_tokens":3,"reasoning_output_tokens":1}}}}`
	if u, ok := GetParser("codex").ParseUsage(strings.NewReader(codex)); !ok || u.InputTokens != 10 || u.OutputTokens != 4 {
		t.Fatalf("codex parser got=%+v ok=%v, want input=10 output=4 true", u, ok)
	}

	claude := `{"type":"assistant","message":{"usage":{"input_tokens":7,"output_tokens":3}}}`
	for _, backend := range []string{"", "unknown"} {
		u, ok := GetParser(backend).ParseUsage(strings.NewReader(claude))
		if !ok || u.InputTokens != 7 || u.OutputTokens != 3 {
			t.Fatalf("backend %q got=%+v ok=%v, want input=7 output=3 true", backend, u, ok)
		}
	}

	antigravity := `{"type":"USER_INPUT","content":"abcd"}`
	if u, ok := GetParser("antigravity").ParseUsage(strings.NewReader(antigravity)); !ok || u.InputTokens != 1 || u.OutputTokens != 0 {
		t.Fatalf("antigravity parser got=%+v ok=%v, want input=1 output=0 true", u, ok)
	}
}
