package spend

import (
	"strings"
	"testing"
)

func TestParseUsageSumsAllTurns(t *testing.T) {
	// Two assistant turns carry usage; the user line and an assistant line with no
	// usage block are ignored. Cumulative input+output is summed across turns.
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":9999}}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":50,"output_tokens":30}}}`
	u, ok := GetParser("claude").ParseUsage(strings.NewReader(jsonl))
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if u.InputTokens != 150 {
		t.Errorf("InputTokens=%d, want 150 (100+50, cache excluded)", u.InputTokens)
	}
	if u.OutputTokens != 50 {
		t.Errorf("OutputTokens=%d, want 50 (20+30)", u.OutputTokens)
	}
	if u.Total() != 200 {
		t.Errorf("Total=%d, want 200", u.Total())
	}
}

func TestParseUsageNoUsageReturnsFalse(t *testing.T) {
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"summary","summary":"x"}`
	if u, ok := GetParser("claude").ParseUsage(strings.NewReader(jsonl)); ok || u.Total() != 0 {
		t.Fatalf("got %+v ok=%v, want zero/false", u, ok)
	}
}

func TestParseUsageSkipsMalformedLines(t *testing.T) {
	// A partial/corrupt line (rotated or mid-write transcript) must not abort the
	// scan — the valid turns around it still sum.
	jsonl := `not json at all
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":40,"output_tokens":10}}}
{ broken`
	u, ok := GetParser("claude").ParseUsage(strings.NewReader(jsonl))
	if !ok || u.InputTokens != 40 || u.OutputTokens != 10 {
		t.Fatalf("got %+v ok=%v, want input40/output10/true", u, ok)
	}
}

func TestParseUsageEmpty(t *testing.T) {
	if _, ok := GetParser("claude").ParseUsage(strings.NewReader("")); ok {
		t.Fatal("ok=true on empty input, want false")
	}
}
