package cli

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/approval"
)

func recognizedView(id string) approval.View {
	opts := []string{"Yes", "Yes, and always allow access to tmp/ from this project", "No"}
	return approval.View{
		ID:          id,
		Question:    "Do you want to proceed?",
		Options:     opts,
		Fingerprint: approval.Fingerprint(opts),
		Recognized:  true,
	}
}

func TestParseOption(t *testing.T) {
	for _, in := range []string{"1", "2", "10"} {
		if _, err := parseOption(in); err != nil {
			t.Fatalf("parseOption(%q) unexpected err: %v", in, err)
		}
	}
	for _, in := range []string{"0", "-3", "x", "", "1.5"} {
		if _, err := parseOption(in); err == nil {
			t.Fatalf("parseOption(%q): expected error", in)
		}
	}
	if n, _ := parseOption("3"); n != 3 {
		t.Fatalf("parseOption(\"3\")=%d, want 3", n)
	}
}

func TestValidateApproval(t *testing.T) {
	views := []approval.View{
		recognizedView("agent-aaa"),
		{ID: "agent-bbb", Recognized: false},
	}

	// happy path returns the matched view
	v, err := validateApproval(views, "agent-aaa", 1)
	if err != nil || v.ID != "agent-aaa" {
		t.Fatalf("happy path: v=%+v err=%v", v, err)
	}

	// not found
	if _, err := validateApproval(views, "agent-zzz", 1); err == nil ||
		!strings.Contains(err.Error(), "no pending approval") {
		t.Fatalf("not-found: got %v", err)
	}

	// found but unrecognized -> attach hint
	if _, err := validateApproval(views, "agent-bbb", 1); err == nil ||
		!strings.Contains(err.Error(), "attach") {
		t.Fatalf("unrecognized: got %v", err)
	}

	// option out of range (high and low)
	if _, err := validateApproval(views, "agent-aaa", 4); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Fatalf("high: got %v", err)
	}
	if _, err := validateApproval(views, "agent-aaa", 0); err == nil {
		t.Fatalf("zero: expected error")
	}
}

func TestFormatApprovalsList(t *testing.T) {
	rec := recognizedView("agent-aaa")
	unrec := approval.View{ID: "agent-bbb", Recognized: false}

	// disabled
	if out := formatApprovalsList(false, nil); !strings.Contains(out, "disabled") {
		t.Fatalf("disabled: got %q", out)
	}

	// enabled, empty
	if out := formatApprovalsList(true, nil); !strings.Contains(out, "no pending approvals") {
		t.Fatalf("empty: got %q", out)
	}

	// enabled, only unrecognized -> no pending + footer count
	onlyUnrec := formatApprovalsList(true, []approval.View{unrec})
	if !strings.Contains(onlyUnrec, "no pending approvals") || !strings.Contains(onlyUnrec, "1 other") {
		t.Fatalf("only-unrecognized: got %q", onlyUnrec)
	}

	// enabled, recognized present -> id, question, numbered options, answer hint
	full := formatApprovalsList(true, []approval.View{rec, unrec})
	for _, want := range []string{"agent-aaa", "Do you want to proceed?", "1. Yes", "3. No", "warden approve agent-aaa", "1 other"} {
		if !strings.Contains(full, want) {
			t.Fatalf("full: missing %q in %q", want, full)
		}
	}
}
