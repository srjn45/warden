package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
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
