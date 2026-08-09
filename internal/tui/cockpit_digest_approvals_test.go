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
	m := newListPane(f, "", "")
	// Populate one agent and put the cursor on it.
	mm, _ := m.Update(sessionsMsg{sessions: []*store.Session{{ID: "agent-1"}}})
	m = mm.(controlPaneModel)

	mm, cmd := m.Update(key("d"))
	m = mm.(controlPaneModel)
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
	m = mm.(controlPaneModel)
	if m.mode != modeDigest {
		t.Fatalf("expected modeDigest after digestMsg, got %v", m.mode)
	}
}

// --- approvals (the `i` key + number answering) ---

func TestItemsIncludesApprovalsRowWhenPending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.apprEnabled = true
	m.approvals = []approval.View{
		{ID: "agent-1", Recognized: true, Options: []string{"Yes", "No"}},
		{ID: "agent-2", Recognized: false}, // unrecognized → not counted
	}
	items := m.items()
	// The Approvals section is always the first row; its count reflects recognized
	// menus only, and the recognized prompt expands to its own row beneath it.
	if len(items) == 0 || items[0].section != secApprovals {
		t.Fatalf("expected the Approvals section header first; got %+v", items)
	}
	if items[0].secCount != 1 {
		t.Errorf("secCount = %d, want 1 (recognized only)", items[0].secCount)
	}
	var apprIDs []string
	for _, it := range items {
		if it.apprView != nil {
			apprIDs = append(apprIDs, it.apprView.ID)
		}
	}
	if len(apprIDs) != 1 || apprIDs[0] != "agent-1" {
		t.Fatalf("approval rows = %v, want exactly [agent-1] (recognized only)", apprIDs)
	}
}

func TestNoApprovalsRowWhenNonePending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.apprEnabled = true // enabled but nothing waiting
	sawHeader := false
	for _, it := range m.items() {
		if it.section == secApprovals {
			sawHeader = true
			if it.secCount != 0 {
				t.Fatalf("Approvals header count = %d, want 0 when nothing pending", it.secCount)
			}
		}
		if it.apprView != nil {
			t.Fatal("no approval rows should appear when no recognized approvals pending")
		}
	}
	if !sawHeader {
		t.Fatal("the Approvals section header must always be present")
	}
}

// Note: the standalone `i` key now opens the agent-detail overlay (modeDetails);
// it no longer enters the approvals overlay. The approvals binding moved to `p`.

func TestKeyPEntersApprovalsModeWhenPending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.apprEnabled = true
	m.approvals = []approval.View{{ID: "agent-1", Recognized: true, Options: []string{"Yes", "No"}}}
	mm, _ := m.Update(key("p"))
	m = mm.(controlPaneModel)
	if m.mode != modeApprovals {
		t.Fatalf("expected modeApprovals after p, got %v", m.mode)
	}
}

func TestKeyPStatusWhenDisabled(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.apprEnabled = false
	mm, _ := m.Update(key("p"))
	m = mm.(controlPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("p should not open the overlay when disabled, got %v", m.mode)
	}
	if !strings.Contains(m.status, "approvals disabled") {
		t.Fatalf("expected an 'approvals disabled' status, got %q", m.status)
	}
}

func TestKeyPStatusWhenEnabledButEmpty(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.apprEnabled = true
	m.approvals = nil
	mm, _ := m.Update(key("p"))
	m = mm.(controlPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("p should not open an empty overlay, got %v", m.mode)
	}
	if !strings.Contains(m.status, "no approvals pending") {
		t.Fatalf("expected 'no approvals pending' status, got %q", m.status)
	}
}

func TestDigitInApprovalsModeAnswersFocusedApproval(t *testing.T) {
	f := &fakeAPI{}
	m := newListPane(f, "", "")
	m.apprEnabled = true
	m.mode = modeApprovals
	m.apprCursor = 0
	m.approvals = []approval.View{
		{ID: "agent-7", Recognized: true, Fingerprint: "fp123", Options: []string{"Yes", "No"}},
	}

	mm, cmd := m.Update(key("1"))
	m = mm.(controlPaneModel)
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
		PermissionMode:  "acceptEdits",
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
		"acceptEdits",                          // permission mode
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

func TestDetailBodyShowsBackend(t *testing.T) {
	s := &store.Session{ID: "agent-1", Status: store.StatusWorking, Backend: "aider"}
	if out := detailBody(s, 80); !strings.Contains(out, "backend") || !strings.Contains(out, "aider") {
		t.Errorf("detailBody() should show the backend line with 'aider':\n%s", out)
	}
}

func TestDetailBodyEmptyBackendDefaultsToClaude(t *testing.T) {
	s := &store.Session{ID: "agent-1", Status: store.StatusWorking}
	if out := detailBody(s, 80); !strings.Contains(out, "backend") || !strings.Contains(out, "claude") {
		t.Errorf("detailBody() should default an empty backend to claude:\n%s", out)
	}
}

func TestKeyIOnAgentEntersDetailsMode(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m = lstep(m, tea.WindowSizeMsg{Width: 80, Height: 24}) // size the viewport (the real cockpit always does)
	mm, _ := m.Update(sessionsMsg{sessions: []*store.Session{
		{ID: "agent-1", Status: store.StatusWorking, Subject: "doing the thing"},
	}})
	m = mm.(controlPaneModel)

	mm, _ = m.Update(key("i"))
	m = mm.(controlPaneModel)
	if m.mode != modeDetails {
		t.Fatalf("expected modeDetails after i on an agent, got %v", m.mode)
	}
	// the detail content should be in the viewport
	if !strings.Contains(m.vp.View(), "agent-1") {
		t.Fatalf("details viewport should render the agent id; got:\n%s", m.vp.View())
	}

	// i again toggles back
	mm, _ = m.Update(key("i"))
	m = mm.(controlPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("expected modeNormal after toggling i again, got %v", m.mode)
	}
}

func TestKeyINoOpWhenNoAgentSelected(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "") // no sessions → cursor on nothing selectable
	mm, _ := m.Update(key("i"))
	m = mm.(controlPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("i should be a no-op with no agent selected, got mode %v", m.mode)
	}
}
