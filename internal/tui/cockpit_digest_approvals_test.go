package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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

// Note: the standalone `i` key now opens the agent-detail overlay (modeDetails);
// it no longer enters the approvals overlay. The approvals binding moves to `p`
// in a later task, which re-adds the corresponding key tests.

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

// --- agent details (the `i` key) ---

func TestDetailBodyRendersAllSections(t *testing.T) {
	s := &store.Session{
		ID:              "agent-9f3c",
		Type:            store.TypeDevelopment,
		Subject:         "Refactor lifecycle reaper retry path",
		Status:          store.StatusWaitingForInput,
		Supervised:      true,
		ContextTokens:   88000,
		ContextState:    store.ContextWarning,
		Repo:            "/Users/me/workspace/warden",
		Branch:          "fix/reaper-retry",
		Worktree:        "/Users/me/workspace/warden/.wt/reaper",
		Ticket:          "WARD-42",
		PR:              "#318",
		PipelineID:      "ctx-guard",
		JobID:           "implement",
		PID:             48213,
		TmuxSession:     "warden-9f3c",
		ClaudeSessionID: "7a1c2d3e-1111-2222-3333-444455556666",
		Prompt:          "Fix the reaper so completed-job records get reaped.",
	}
	out := detailBody(s, 80)
	for _, want := range []string{
		"agent-9f3c",                           // header id
		"Refactor lifecycle reaper retry path", // subject
		"88k",                                  // context figure
		"fix/reaper-retry",                     // branch
		"WARD-42",                              // ticket
		"#318",                                 // pr
		"ctx-guard",                            // pipeline
		"supervised",                           // mode
		"48213",                                // pid
	} {
		if !strings.Contains(out, want) {
			t.Errorf("detailBody() missing %q in:\n%s", want, out)
		}
	}
}

func TestDetailBodyOmitsEmptySections(t *testing.T) {
	s := &store.Session{ID: "agent-1", Status: store.StatusWorking, PID: 10, TmuxSession: "warden-1"}
	out := detailBody(s, 80)
	for _, absent := range []string{"ticket", "pr ", "worktree", "pipeline"} {
		if strings.Contains(out, absent) {
			t.Errorf("detailBody() should omit %q for an agent without that data:\n%s", absent, out)
		}
	}
}

func TestDetailBodyHandlesNil(t *testing.T) {
	if got := detailBody(nil, 80); !strings.Contains(got, "no agent") {
		t.Errorf("detailBody(nil) = %q, want a 'no agent' placeholder", got)
	}
}

func TestKeyIOnAgentEntersDetailsMode(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m = lstep(m, tea.WindowSizeMsg{Width: 80, Height: 24}) // size the viewport (the real cockpit always does)
	mm, _ := m.Update(sessionsMsg{sessions: []*store.Session{
		{ID: "agent-1", Status: store.StatusWorking, Subject: "doing the thing"},
	}})
	m = mm.(listPaneModel)

	mm, _ = m.Update(key("i"))
	m = mm.(listPaneModel)
	if m.mode != modeDetails {
		t.Fatalf("expected modeDetails after i on an agent, got %v", m.mode)
	}
	// the detail content should be in the viewport
	if !strings.Contains(m.vp.View(), "agent-1") {
		t.Fatalf("details viewport should render the agent id; got:\n%s", m.vp.View())
	}

	// i again toggles back
	mm, _ = m.Update(key("i"))
	m = mm.(listPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("expected modeNormal after toggling i again, got %v", m.mode)
	}
}

func TestKeyINoOpWhenNoAgentSelected(t *testing.T) {
	m := newListPane(&fakeAPI{}, "") // no sessions → cursor on nothing selectable
	mm, _ := m.Update(key("i"))
	m = mm.(listPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("i should be a no-op with no agent selected, got mode %v", m.mode)
	}
}
