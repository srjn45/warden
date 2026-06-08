package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/warden/internal/client"
)

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := oneLine("a\n\tb   c\n")
	if got != "a b c" {
		t.Fatalf("want %q, got %q", "a b c", got)
	}
}

func TestCtxLineTruncatesValue(t *testing.T) {
	e := client.ContextEntry{Key: "global.foo", Value: "hello world this is long"}
	line := ctxLine(e, 8)
	if !strings.Contains(line, "global.foo") {
		t.Fatalf("line missing key: %q", line)
	}
	if strings.Contains(line, "this is long") {
		t.Fatalf("value should be truncated: %q", line)
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("truncated value should end with ellipsis: %q", line)
	}
}

func TestCtxLineCollapsesMultilineValue(t *testing.T) {
	e := client.ContextEntry{Key: "k", Value: "line1\nline2"}
	line := ctxLine(e, 80)
	if strings.Contains(line, "\n") {
		t.Fatalf("value newlines must be collapsed: %q", line)
	}
	if !strings.Contains(line, "line1 line2") {
		t.Fatalf("want collapsed value, got %q", line)
	}
}

func TestMsgLineShowsFromToAndBody(t *testing.T) {
	m := client.Message{From: "agent-1", To: "agent-2", Body: "ship it"}
	line := msgLine(m, 80)
	if !strings.Contains(line, "agent-1") || !strings.Contains(line, "agent-2") {
		t.Fatalf("line missing endpoints: %q", line)
	}
	if !strings.Contains(line, "→") {
		t.Fatalf("want a direction arrow: %q", line)
	}
	if !strings.Contains(line, "ship it") {
		t.Fatalf("line missing body: %q", line)
	}
}

func TestMsgLineTruncatesBody(t *testing.T) {
	m := client.Message{From: "a", To: "b", Body: "x123456789"}
	line := msgLine(m, 4)
	if strings.Contains(line, "x123456789") {
		t.Fatalf("body should be truncated: %q", line)
	}
}

func TestInspectorBodyHasSectionsAndCounts(t *testing.T) {
	entries := []client.ContextEntry{{Key: "k1", Value: "v1"}}
	msgs := []client.Message{{From: "a", To: "b", Body: "hi"}}
	body := inspectorBody(entries, msgs, 80)
	if !strings.Contains(body, "Shared context") {
		t.Fatalf("missing context header: %q", body)
	}
	if !strings.Contains(body, "Messages") {
		t.Fatalf("missing messages header: %q", body)
	}
	if !strings.Contains(body, "k1") || !strings.Contains(body, "hi") {
		t.Fatalf("missing content: %q", body)
	}
}

func TestInspectorBodyEmptyStates(t *testing.T) {
	body := inspectorBody(nil, nil, 80)
	if !strings.Contains(strings.ToLower(body), "no shared context") {
		t.Fatalf("want empty context hint: %q", body)
	}
	if !strings.Contains(strings.ToLower(body), "no messages") {
		t.Fatalf("want empty messages hint: %q", body)
	}
}
