package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/knownprompts"
)

func TestFormatKnownPrompts(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if got := formatKnownPrompts(nil, now); got != "(no known prompts)\n" {
		t.Fatalf("empty = %q", got)
	}
	out := formatKnownPrompts([]knownprompts.Entry{{
		ID: "abc123", Backend: "claude", Question: "Allow {{action}}?",
		Options: []string{"Yes", "No"}, Affirmative: 1, Hits: 7, LastSeenAt: now.Add(-3 * time.Hour),
	}}, now)
	for _, want := range []string{"ID", "LAST SEEN", "abc123", "claude", "1. Yes *", "2. No", "7", "3h ago"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestApprovalKnownListCmd(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/known-prompts" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"prompts":[{"id":"abc","backend":"claude","question":"Q?","options":["Yes","No"],"affirmative":1,"hits":2}]}`))
	})
	out, err := runCLI(t, addr, "approval", "known", "list")
	if err != nil || !strings.Contains(out, "abc") || !strings.Contains(out, "1. Yes *") {
		t.Fatalf("list: err=%v out=%q", err, out)
	}
	out, err = runCLI(t, addr, "approval", "known", "list", "--json")
	if err != nil || !strings.Contains(out, `"id": "abc"`) || !strings.Contains(out, `"hits": 2`) {
		t.Fatalf("list --json: err=%v out=%q", err, out)
	}
}

func TestApprovalKnownForget(t *testing.T) {
	var hits []string
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/v1/known-prompts" {
			_, _ = w.Write([]byte(`{"removed":3}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"forgotten"}`))
	})
	if out, err := runCLI(t, addr, "approval", "known", "forget", "abc"); err != nil || !strings.Contains(out, "forgot abc") {
		t.Fatalf("forget one: err=%v out=%q", err, out)
	}
	if out, err := runCLI(t, addr, "approval", "known", "forget", "--all", "--yes"); err != nil || !strings.Contains(out, "forgot 3") {
		t.Fatalf("forget --all --yes: err=%v out=%q", err, out)
	}
	if _, err := runCLI(t, addr, "approval", "known", "forget"); err == nil {
		t.Fatal("forget with no id and no --all must error")
	}
	if _, err := runCLI(t, addr, "approval", "known", "forget", "abc", "--all"); err == nil {
		t.Fatal("id plus --all must error")
	}
	want := "DELETE /api/v1/known-prompts/abc,DELETE /api/v1/known-prompts"
	if strings.Join(hits, ",") != want {
		t.Fatalf("hits = %v", hits)
	}
}

func TestConfirmForgetAll(t *testing.T) {
	var sink strings.Builder
	if !confirmForgetAll(strings.NewReader("y\n"), &sink) || !confirmForgetAll(strings.NewReader("YES\n"), &sink) {
		t.Fatal("y/yes must confirm")
	}
	if confirmForgetAll(strings.NewReader("\n"), &sink) || confirmForgetAll(strings.NewReader("n\n"), &sink) {
		t.Fatal("default/n must decline")
	}
}
