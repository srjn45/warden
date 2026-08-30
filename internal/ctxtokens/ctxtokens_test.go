package ctxtokens

import (
	"strings"
	"testing"
)

func TestLatestContextTokensSumsLatestUsage(t *testing.T) {
	// Two assistant turns; the LAST one's usage is what counts.
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":3}}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":7,"output_tokens":4}}}`
	got, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl))
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if want := 20 + 300 + 7; got != want {
		t.Fatalf("tokens=%d, want %d", got, want)
	}
}

func TestLatestContextTokensNoUsageReturnsFalse(t *testing.T) {
	// Freshly spawned: a user line but no assistant turn with usage yet.
	jsonl := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"summary","summary":"x"}`
	if _, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl)); ok {
		t.Fatal("ok=true, want false (no usage)")
	}
}

func TestLatestContextTokensSkipsMalformedLines(t *testing.T) {
	jsonl := `not json at all
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
{ broken`
	got, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 50 {
		t.Fatalf("got=%d ok=%v, want 50 true", got, ok)
	}
}

func TestLatestContextTokensCompactBoundaryResetsGauge(t *testing.T) {
	// A landed /compact writes a compact_boundary line but NO new assistant
	// turn until the next prompt — the boundary's postTokens is the current
	// fill, not the stale pre-compact assistant usage above it.
	jsonl := `{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":20,"cache_read_input_tokens":181000,"cache_creation_input_tokens":705}}}
{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"manual","preTokens":181725,"postTokens":12062}}
{"type":"user","message":{"role":"user","content":"This session is being continued..."},"isCompactSummary":true}`
	got, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 12062 {
		t.Fatalf("got=%d ok=%v, want 12062 true", got, ok)
	}
}

func TestLatestContextTokensAssistantAfterBoundaryWins(t *testing.T) {
	// Once the next prompt produces a fresh assistant turn, its usage is the
	// gauge again — the boundary only covers the gap.
	jsonl := `{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":10,"cache_read_input_tokens":180000,"cache_creation_input_tokens":0}}}
{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":180010,"postTokens":9000}}
{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":15,"cache_read_input_tokens":40000,"cache_creation_input_tokens":100}}}`
	got, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 15+40000+100 {
		t.Fatalf("got=%d ok=%v, want %d true", got, ok, 15+40000+100)
	}
}

func TestLatestContextTokensBoundaryWithoutMetadata(t *testing.T) {
	// A boundary missing compactMetadata still marks a reset — an unmeasured
	// one reads as 0, which beats reporting the stale critical level.
	jsonl := `{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":10,"cache_read_input_tokens":180000,"cache_creation_input_tokens":0}}}
{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}`
	got, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 0 {
		t.Fatalf("got=%d ok=%v, want 0 true", got, ok)
	}
}

func TestLatestContextTokensOtherSystemLinesIgnored(t *testing.T) {
	// Non-boundary system lines must not disturb the gauge.
	jsonl := `{"type":"assistant","message":{"role":"assistant","usage":{"input_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
{"type":"system","subtype":"local_command","content":"..."}`
	got, ok := GetParser("claude").LatestContextTokens(strings.NewReader(jsonl))
	if !ok || got != 50 {
		t.Fatalf("got=%d ok=%v, want 50 true", got, ok)
	}
}

func TestClassify(t *testing.T) {
	const warn, crit = 200, 400
	cases := []struct {
		tokens int
		want   State
	}{
		{0, StateOK},
		{199, StateOK},
		{200, StateWarning}, // boundary: warn is inclusive
		{399, StateWarning},
		{400, StateCritical}, // boundary: crit is inclusive
		{1000, StateCritical},
	}
	for _, c := range cases {
		if got := Classify(c.tokens, warn, crit); got != c.want {
			t.Errorf("Classify(%d)=%q, want %q", c.tokens, got, c.want)
		}
	}
}
