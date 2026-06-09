package tui

import (
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/store"
)

// --- digest (the `d` key) ---

func TestDigestBodyRendersKeyFields(t *testing.T) {
	d := &digest.Digest{
		Summary: "Refactored the auth middleware and added tests",
		Branch:  "feature/auth",
		Turns:   12,
		Status:  "done",
		Task:    "review the auth module",
		Files: []digest.FileChange{
			{Path: "internal/auth/mw.go", Added: 40, Removed: 12, Edited: true},
		},
	}
	out := digestBody(d, 80)
	for _, want := range []string{
		"Refactored the auth middleware", // summary
		"feature/auth",                   // branch
		"12",                             // turns
		"internal/auth/mw.go",            // file path
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digestBody() missing %q in:\n%s", want, out)
		}
	}
}

func TestKeyDOnAgentFetchesDigestAndEntersDigestMode(t *testing.T) {
	f := &fakeAPI{digest: &digest.Digest{Summary: "did the thing", Branch: "b", Turns: 3}}
	m := newListPane(f, "")
	// Populate one agent and put the cursor on it.
	mm, _ := m.Update(sessionsMsg{sessions: []*store.Session{{ID: "agent-1"}}})
	m = mm.(listPaneModel)

	mm, cmd := m.Update(key("d"))
	m = mm.(listPaneModel)
	if cmd == nil {
		t.Fatal("pressing d produced no command")
	}
	msg := cmd() // drives fakeAPI.Digest
	dm, ok := msg.(digestMsg)
	if !ok {
		t.Fatalf("expected digestMsg, got %T", msg)
	}
	if dm.id != "agent-1" || dm.d == nil || dm.d.Summary != "did the thing" {
		t.Fatalf("digestMsg did not carry the fetched digest: %+v", dm)
	}
	mm, _ = m.Update(dm)
	m = mm.(listPaneModel)
	if m.mode != modeDigest {
		t.Fatalf("expected modeDigest after digestMsg, got %v", m.mode)
	}
}

// --- approvals (the `i` key + number answering) ---

func TestItemsIncludesApprovalsRowWhenPending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = true
	m.approvals = []approval.View{
		{ID: "agent-1", Recognized: true, Options: []string{"Yes", "No"}},
		{ID: "agent-2", Recognized: false}, // unrecognized → not counted
	}
	items := m.items()
	if len(items) == 0 || !items[0].approvals {
		t.Fatalf("expected an approvals row first; got %+v", items)
	}
	if items[0].apprCount != 1 {
		t.Errorf("apprCount = %d, want 1 (recognized only)", items[0].apprCount)
	}
}

func TestNoApprovalsRowWhenNonePending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = true // enabled but nothing waiting
	for _, it := range m.items() {
		if it.approvals {
			t.Fatal("approvals row should not appear when no recognized approvals pending")
		}
	}
}

func TestKeyIEntersApprovalsModeWhenPending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = true
	m.approvals = []approval.View{{ID: "agent-1", Recognized: true, Options: []string{"Yes", "No"}}}

	mm, _ := m.Update(key("i"))
	m = mm.(listPaneModel)
	if m.mode != modeApprovals {
		t.Fatalf("expected modeApprovals after i, got %v", m.mode)
	}
}

func TestKeyIDoesNothingWhenNoApprovals(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = true
	mm, _ := m.Update(key("i"))
	if mm.(listPaneModel).mode != modeNormal {
		t.Fatal("i should be a no-op when there are no pending approvals")
	}
}

func TestDigitInApprovalsModeAnswersFocusedApproval(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "")
	m.apprEnabled = true
	m.mode = modeApprovals
	m.apprCursor = 0
	m.approvals = []approval.View{
		{ID: "agent-7", Recognized: true, Fingerprint: "fp123", Options: []string{"Yes", "No"}},
	}

	mm, cmd := m.Update(key("1"))
	m = mm.(listPaneModel)
	if cmd == nil {
		t.Fatal("pressing 1 in approvals mode produced no command")
	}
	cmd() // drives fakeAPI.Approve
	if f.approvedID != "agent-7" || f.approvedOpt != 1 || f.approvedFP != "fp123" {
		t.Fatalf("Approve called with (%q,%d,%q), want (agent-7,1,fp123)", f.approvedID, f.approvedOpt, f.approvedFP)
	}
}

func TestApprovalsCmdFetchesEnabledAndViews(t *testing.T) {
	f := &fakeAPI{approvalsOn: true, approvals: []approval.View{{ID: "a", Recognized: true}}}
	msg := approvalsCmd(f)()
	am, ok := msg.(approvalsMsg)
	if !ok {
		t.Fatalf("expected approvalsMsg, got %T", msg)
	}
	if !am.enabled || len(am.views) != 1 {
		t.Fatalf("approvalsMsg = %+v, want enabled with 1 view", am)
	}
}
