package ctxtokens

import (
	"strings"
	"testing"
)

func TestCodexLatestContextTokensUsesLastSnapshot(t *testing.T) {
	jsonl := `not json
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":5,"output_tokens":2,"reasoning_output_tokens":1,"total_tokens":18}}}}
{"type":"event_msg","payload":{"type":"agent_message"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"cached_input_tokens":10,"output_tokens":4,"reasoning_output_tokens":2,"total_tokens":46}}}}
{ broken`

	got, ok := GetParser("codex").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 46 {
		t.Fatalf("got=%d ok=%v, want 46 true", got, ok)
	}
}

func TestCodexLatestContextTokensFallsBackToComponents(t *testing.T) {
	jsonl := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30,"cached_input_tokens":10,"output_tokens":4,"reasoning_output_tokens":2}}}}`
	got, ok := GetParser("codex").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 46 {
		t.Fatalf("got=%d ok=%v, want 46 true", got, ok)
	}
}

func TestCodexLatestContextTokensNoUsage(t *testing.T) {
	jsonl := `{"type":"event_msg","payload":{"type":"token_count"}}
{"type":"event_msg","payload":{"type":"agent_message"}}`
	if _, ok := GetParser("codex").LatestContextTokens(strings.NewReader(jsonl)); ok {
		t.Fatal("ok=true, want false")
	}
}

func TestContextParserSelectionPreservesClaudeFallback(t *testing.T) {
	codex := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":12}}}}`
	if got, ok := GetParser("codex").LatestContextTokens(strings.NewReader(codex)); !ok || got != 12 {
		t.Fatalf("codex parser got=%d ok=%v, want 12 true", got, ok)
	}

	claude := `{"type":"assistant","message":{"usage":{"input_tokens":7}}}`
	for _, backend := range []string{"", "unknown"} {
		if got, ok := GetParser(backend).LatestContextTokens(strings.NewReader(claude)); !ok || got != 7 {
			t.Fatalf("backend %q got=%d ok=%v, want 7 true", backend, got, ok)
		}
	}

	antigravity := `{"type":"USER_INPUT","content":"abcd"}`
	if got, ok := GetParser("antigravity").LatestContextTokens(strings.NewReader(antigravity)); !ok || got != 1 {
		t.Fatalf("antigravity parser got=%d ok=%v, want 1 true", got, ok)
	}
}
