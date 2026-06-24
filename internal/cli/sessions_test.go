package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/store"
)

func TestPrintJSON_EmptySlice(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, []store.Session{}); err != nil {
		t.Fatalf("printJSON returned error: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Fatalf("empty slice: want %q, got %q", "[]", got)
	}
}

func TestPrintJSON_SessionHasFields(t *testing.T) {
	var buf bytes.Buffer
	s := store.Session{ID: "agent-x1", Status: store.Status("working")}
	if err := printJSON(&buf, s); err != nil {
		t.Fatalf("printJSON returned error: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if round["id"] != "agent-x1" {
		t.Fatalf("want id=agent-x1, got %v", round["id"])
	}
	if !strings.Contains(buf.String(), "\n  \"id\"") {
		t.Fatalf("expected 2-space indented output, got:\n%s", buf.String())
	}
}

func TestRenderSessions(t *testing.T) {
	var buf bytes.Buffer
	sessions := []*store.Session{
		{ID: "A-1", Name: "alpha", Type: store.Type("development"), Status: store.Status("working"), Subject: "do a thing"},
		{ID: "B-2"}, // sparse session exercises the em-dash fallbacks
	}
	if err := renderSessions(&buf, sessions, false); err != nil {
		t.Fatalf("renderSessions returned error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "ID", "SUBJECT", "alpha", "A-1", "B-2", "do a thing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The unnamed B-2 row falls back to "—" for name and "…" for a pending type.
	if !strings.Contains(out, "—") || !strings.Contains(out, "…") {
		t.Errorf("expected fallback glyphs for sparse row:\n%s", out)
	}
}

func TestContextCell(t *testing.T) {
	cases := []struct {
		tokens int
		state  string
		want   string
	}{
		{0, "", "—"},
		{145000, "ok", "145k"},
		{210000, "warning", "210k"},
		{410000, "critical", "410k"},
	}
	for _, c := range cases {
		if got := contextCell(c.tokens, c.state, false); got != c.want {
			t.Errorf("contextCell(%d,%q)=%q, want %q", c.tokens, c.state, got, c.want)
		}
	}
}
