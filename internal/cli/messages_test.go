package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/client"
)

func TestResolveSenderPrecedence(t *testing.T) {
	if resolveSender("flag", "env") != "flag" {
		t.Fatal("--as should win")
	}
	if resolveSender("", "env") != "env" {
		t.Fatal("env next")
	}
	if resolveSender("", "") != "human" {
		t.Fatal("default human")
	}
}

func TestResolveSelfRequiresID(t *testing.T) {
	if v, err := resolveSelf("flag", "env"); err != nil || v != "flag" {
		t.Fatalf("--as: v=%q err=%v", v, err)
	}
	if v, err := resolveSelf("", "env"); err != nil || v != "env" {
		t.Fatalf("env: v=%q err=%v", v, err)
	}
	if _, err := resolveSelf("", ""); err == nil {
		t.Fatal("expected error when no id available")
	}
}

func TestFormatMessage(t *testing.T) {
	m := client.Message{From: "agent-A", Body: "hi there", Read: false,
		TS: time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)}
	out := formatMessage(m)
	if !strings.Contains(out, "agent-A") || !strings.Contains(out, "hi there") || !strings.Contains(out, "[unread]") {
		t.Fatalf("got %q", out)
	}
	read := client.Message{From: "agent-A", Body: "x", Read: true, TS: m.TS}
	if strings.Contains(formatMessage(read), "[unread]") {
		t.Fatalf("read message should not show [unread]")
	}
}
