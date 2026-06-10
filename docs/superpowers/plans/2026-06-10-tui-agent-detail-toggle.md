# TUI agent-detail toggle + approvals key rebind — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lean the cockpit agent row to four never-clipping columns (ID/status/context/age), add an `i`-toggled same-pane detail overlay showing every `store.Session` field, and move approvals off `i` to `p` with feedback when the queue is empty.

**Architecture:** The list pane is a single bubble-tea model (`listPaneModel`) with a `mode` field. Overlay modes (`modeInspector`, `modeDigest`, `modeApprovals`) each replace `renderList(...)` inside the framed `titleBox` in `View()` and toggle back to `modeNormal`, rendering scrollable content via the shared `m.vp` viewport. This change adds one more such mode (`modeDetails`) following the `modeDigest` pattern exactly, leans the row renderer, and rebinds one key.

**Tech Stack:** Go, bubbletea/bubbles (viewport), lipgloss, testify. Tests live in `internal/tui/*_test.go` and run with `go test ./internal/tui/...`.

**Spec:** `docs/superpowers/specs/2026-06-10-tui-agent-detail-toggle-design.md`

**Working directory:** the worktree `/Users/srajan.pathak/workspace/personal/warden/.claude/worktrees/tui-agent-detail` (branch `tui-agent-detail`). All paths below are relative to it. Commits stay on this branch.

---

### Task 1: Lean the agent row to ID/status/context/age

**Files:**
- Modify: `internal/tui/list.go` (the `default` case of `renderItemLine`, currently lines 507-513)
- Test: `internal/tui/list_test.go` (update `TestRenderListContainsAgeColumn`, lines 27-41; add a no-clip test)

- [ ] **Step 1: Update the row-render test to the new contract**

In `internal/tui/list_test.go`, replace `TestRenderListContainsAgeColumn` (lines 27-41) with a test that asserts the four kept columns are present and that the subject is NOT in the row, even when present on the session:

```go
func TestRenderListRowShowsLeanColumns(t *testing.T) {
	sessions := []*store.Session{
		{
			ID:        "agent-abc",
			Status:    store.StatusWorking,
			UpdatedAt: time.Now().Add(-30 * time.Second), // 30s ago → "<1m"
			Subject:   "this subject must not appear in the row",
		},
	}
	out := renderList(buildItems(sessions, nil), 0, 120, 10)
	require.Contains(t, out, "agent-abc", "row should show the ID")
	require.Contains(t, out, "<1m", "row should show the age token")
	require.NotContains(t, out, "this subject", "subject must not be rendered in the lean row")
}

func TestRenderListRowDoesNotClipAtNarrowWidth(t *testing.T) {
	sessions := []*store.Session{
		{
			ID:            "agent-abc",
			Status:        store.StatusWaitingForInput,
			ContextTokens: 88000,
			ContextState:  store.ContextWarning,
			UpdatedAt:     time.Now().Add(-2 * time.Minute), // "2m"
		},
	}
	// Width 30 is narrower than the old 51-char fixed block; the lean row
	// (~36 visible chars of fixed columns) must still carry ID + ctx + age.
	out := renderList(buildItems(sessions, nil), 0, 30, 10)
	require.Contains(t, out, "agent-abc")
	require.Contains(t, out, "88k")
	require.Contains(t, out, "2m")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestRenderListRow' -v`
Expected: FAIL — `TestRenderListRowShowsLeanColumns` fails on `NotContains` (subject still rendered) and the narrow-width test likely fails because the old `width-51` slice clips the columns.

- [ ] **Step 3: Lean the renderItemLine default case**

In `internal/tui/list.go`, replace the `default:` case body (lines 507-513) with the four-column form — drop `Type` and `Subject`:

```go
	default:
		s := it.session
		label, st := badge(s.Status, s.ExitCode)
		cl, cst := contextLabel(s.ContextTokens, s.ContextState)
		line = fmt.Sprintf("%-12s %-11s %-6s %-5s",
			trunc(s.ID, 12), st.Render(label),
			cst.Render(fmt.Sprintf("%-6s", cl)), age(s.UpdatedAt))
```

Note: `width` is now unused by this case but is still used by the function signature/other cases — leave the parameter. `typeOr` stays defined (still referenced by Task 2's `detailBody`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestRenderListRow' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Run the full tui suite and fix any other row-assuming tests**

Run: `go test ./internal/tui/ 2>&1 | tail -20`
Expected: PASS. If any other test asserts a subject/type substring inside a list row, update it to the lean contract (search: `grep -rn "subject\|Subject" internal/tui/*_test.go`). Do not weaken unrelated assertions.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): lean the cockpit agent row to ID/status/context/age"
```

---

### Task 2: `detailBody` render function

**Files:**
- Modify: `internal/tui/list.go` (add `detailBody` near `digestBody`, ~line 538)
- Test: `internal/tui/cockpit_digest_approvals_test.go` (add `detailBody` tests)

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/cockpit_digest_approvals_test.go` (it already imports `store`, `strings`, `testing`):

```go
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
		"agent-9f3c",                            // header id
		"Refactor lifecycle reaper retry path",  // subject
		"88k",                                   // context figure
		"fix/reaper-retry",                      // branch
		"WARD-42",                               // ticket
		"#318",                                  // pr
		"ctx-guard",                             // pipeline
		"supervised",                            // mode
		"48213",                                 // pid
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestDetailBody' -v`
Expected: FAIL — `detailBody` undefined (compile error).

- [ ] **Step 3: Implement `detailBody`**

In `internal/tui/list.go`, add immediately before `digestBody` (~line 538):

```go
// detailBody renders the modeDetails overlay: every populated store.Session
// field for the selected agent, grouped (header, summary, location, refs, mode,
// plumbing). Empty fields/sections are omitted so the view stays tight. Pure
// over the session — no fetch. Reuses badge/contextLabel/age/abbrevHome/trunc.
func detailBody(s *store.Session, width int) string {
	if s == nil {
		return stMuted.Render("(no agent selected)")
	}
	var b strings.Builder
	label, st := badge(s.Status, s.ExitCode)
	permMode := "bypass"
	if s.Supervised {
		permMode = "supervised"
	}
	b.WriteString(stPaneTitle.Render(s.ID) + "  " + st.Render(label) + " · " + permMode + "\n\n")

	// summary block
	if s.Subject != "" {
		b.WriteString(stMuted.Render("subject   ") + s.Subject + "\n")
	}
	b.WriteString(stMuted.Render("type      ") + typeOr(s) + "   " + stMuted.Render("age ") + age(s.UpdatedAt) + "\n")
	if cl, _ := contextLabel(s.ContextTokens, s.ContextState); cl != "" {
		ctxLine := stMuted.Render("context   ") + cl
		if s.ContextState != "" {
			ctxLine += " (" + s.ContextState + ")"
		}
		b.WriteString(ctxLine + "\n")
	}

	// location
	var loc []string
	dir := s.Repo
	if dir == "" {
		dir = s.Workdir
	}
	if dir != "" {
		loc = append(loc, stMuted.Render("  dir       ")+abbrevHome(dir))
	}
	if s.Branch != "" {
		loc = append(loc, stMuted.Render("  branch    ")+s.Branch)
	}
	if s.Worktree != "" {
		loc = append(loc, stMuted.Render("  worktree  ")+abbrevHome(s.Worktree))
	}
	if len(loc) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("location") + "\n" + strings.Join(loc, "\n") + "\n")
	}

	// refs
	var refs []string
	if s.Ticket != "" {
		refs = append(refs, stMuted.Render("  ticket    ")+s.Ticket)
	}
	if s.PR != "" {
		refs = append(refs, stMuted.Render("  pr        ")+s.PR)
	}
	if s.PipelineID != "" {
		line := stMuted.Render("  pipeline  ") + s.PipelineID
		if s.JobID != "" {
			line += "  " + stMuted.Render("job ") + s.JobID
		}
		refs = append(refs, line)
	}
	if len(refs) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("refs") + "\n" + strings.Join(refs, "\n") + "\n")
	}

	// mode
	if s.AutoRestart {
		b.WriteString("\n" + stPaneTitle.Render("mode") + "\n")
		b.WriteString(fmt.Sprintf("  %s · auto-restart ×%d\n", permMode, s.RestartCount))
	}

	// plumbing
	b.WriteString("\n" + stPaneTitle.Render("plumbing") + "\n")
	b.WriteString(fmt.Sprintf("  pid %d · tmux %s\n", s.PID, s.TmuxSession))
	if s.ClaudeSessionID != "" {
		b.WriteString(stMuted.Render("  claude    ") + trunc(s.ClaudeSessionID, 12) + "\n")
	}
	if s.Prompt != "" {
		b.WriteString(stMuted.Render("  prompt    ") + "\"" + trunc(s.Prompt, max(0, width-14)) + "\"\n")
	}
	return b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestDetailBody' -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/cockpit_digest_approvals_test.go
git commit -m "feat(tui): add detailBody render for the agent-detail overlay"
```

---

### Task 3: `modeDetails` — enum, handler, normal-mode `i`, View

**Files:**
- Modify: `internal/tui/model.go` (add `modeDetails` to the enum, after line 49)
- Modify: `internal/tui/list_pane.go` (mode-switch handler ~after line 492; normal-mode `i` case currently at lines 630-634; `View()` ~after line 669)
- Test: `internal/tui/cockpit_digest_approvals_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/cockpit_digest_approvals_test.go`:

```go
func TestKeyIOnAgentEntersDetailsMode(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestKeyIOnAgent|TestKeyINoOp' -v`
Expected: FAIL — `modeDetails` undefined (compile error).

- [ ] **Step 3: Add `modeDetails` to the enum**

In `internal/tui/model.go`, add a line after `modeApprovals` (line 49):

```go
	modeApprovals             // answer pending tool-permission prompts
	modeDetails               // scrollable full detail view for the selected agent
```

- [ ] **Step 4: Add the `modeDetails` key handler**

In `internal/tui/list_pane.go`, add a new case to the mode switch, immediately after the `modeDigest` block (after line 492, before `case modeApprovals:`). Copy the digest shape, swapping the toggle key to `i`:

```go
	case modeDetails:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Sequence(killCockpitCmd(), tea.Quit)
		case "esc", "i":
			m.mode = modeNormal
			return m, nil
		case "g":
			m.vp.GotoTop()
			return m, nil
		case "G":
			m.vp.GotoBottom()
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
```

- [ ] **Step 5: Repurpose the normal-mode `i` case to open details**

In `internal/tui/list_pane.go`, replace the existing normal-mode `case "i":` block (lines 630-634, the old approvals binding) with:

```go
	case "i":
		if s := m.selected(); s != nil {
			m.mode = modeDetails
			m.vp.SetContent(detailBody(s, m.vp.Width))
			m.vp.GotoTop()
		}
```

- [ ] **Step 6: Add the `modeDetails` branch to View()**

In `internal/tui/list_pane.go`, add after the `modeDigest` branch (after line 669, before the `modeApprovals` branch at 671):

```go
	if m.mode == modeDetails {
		body := titleBox("Details — "+m.selectedID(), m.vp.View(), m.w, bodyH)
		return header + "\n" + body + "\n" + stMuted.Render("↑/↓ pgup/pgdn g/G scroll · i/esc back · q quit")
	}
```

- [ ] **Step 7: Run the test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestKeyIOnAgent|TestKeyINoOp' -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/model.go internal/tui/list_pane.go internal/tui/cockpit_digest_approvals_test.go
git commit -m "feat(tui): add i-toggled agent-detail overlay (modeDetails)"
```

---

### Task 4: Move approvals to `p` + empty-queue feedback

**Files:**
- Modify: `internal/tui/list_pane.go` (add normal-mode `case "p":`; change `modeApprovals` toggle-back key at line 498)
- Test: `internal/tui/cockpit_digest_approvals_test.go` (rewrite the two `i`-approvals tests for `p`)

- [ ] **Step 1: Rewrite the approvals key tests for `p`**

In `internal/tui/cockpit_digest_approvals_test.go`, replace `TestKeyIEntersApprovalsModeWhenPending` (lines 93-103) and `TestKeyIDoesNothingWhenNoApprovals` (lines 105-112) with:

```go
func TestKeyPEntersApprovalsModeWhenPending(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = true
	m.approvals = []approval.View{{ID: "agent-1", Recognized: true, Options: []string{"Yes", "No"}}}
	mm, _ := m.Update(key("p"))
	m = mm.(listPaneModel)
	if m.mode != modeApprovals {
		t.Fatalf("expected modeApprovals after p, got %v", m.mode)
	}
}

func TestKeyPStatusWhenDisabled(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = false
	mm, _ := m.Update(key("p"))
	m = mm.(listPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("p should not open the overlay when disabled, got %v", m.mode)
	}
	if !strings.Contains(m.status, "WARDEN_APPROVALS") {
		t.Fatalf("expected a 'set WARDEN_APPROVALS' status, got %q", m.status)
	}
}

func TestKeyPStatusWhenEnabledButEmpty(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.apprEnabled = true
	m.approvals = nil
	mm, _ := m.Update(key("p"))
	m = mm.(listPaneModel)
	if m.mode != modeNormal {
		t.Fatalf("p should not open an empty overlay, got %v", m.mode)
	}
	if !strings.Contains(m.status, "no approvals pending") {
		t.Fatalf("expected 'no approvals pending' status, got %q", m.status)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestKeyP' -v`
Expected: FAIL — `p` is unbound, so mode stays normal and `m.status` is empty (the assertions on status fail; pending-test fails because mode stays normal).

- [ ] **Step 3: Add the normal-mode `p` handler**

In `internal/tui/list_pane.go`, add a new `case "p":` in the normal-mode switch (place it next to where the old `i` case was, e.g. before `case "?":`):

```go
	case "p":
		if len(recognizedApprovals(m.approvals)) > 0 {
			m.mode = modeApprovals
			m.apprCursor = 0
		} else if !m.apprEnabled {
			m.status = "approvals disabled (set WARDEN_APPROVALS)"
		} else {
			m.status = "no approvals pending"
		}
```

- [ ] **Step 4: Update the modeApprovals toggle-back key**

In `internal/tui/list_pane.go`, change the `modeApprovals` handler's toggle key (line 498) from `i` to `p`:

```go
		case "esc", "p":
			m.mode = modeNormal
			return m, nil
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestKeyP' -v`
Expected: PASS (all three).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/list_pane.go internal/tui/cockpit_digest_approvals_test.go
git commit -m "feat(tui): move approvals to p with empty/disabled feedback"
```

---

### Task 5: Update help text and footer teaser

**Files:**
- Modify: `internal/tui/view.go` (`helpText`, lines 23-41)
- Modify: `internal/tui/list_pane.go` (footer teaser, line 680)

- [ ] **Step 1: Update the help text**

In `internal/tui/view.go` `helpText()`, change the approvals line and add a details line. Replace the `i` line (currently around line 35) and keep alphabetic-ish grouping:

```go
		"  d            completion digest for the selected agent (scrollable; d/esc to close)\n" +
		"  i            agent details for the selected agent (scrollable; i/esc to close)\n" +
		"  p            answer pending approvals (or enter on the ⏳ row; 1-9 to answer, tab for next)\n" +
		"  c            shared-context + message-traffic inspector\n" +
```

- [ ] **Step 2: Update the footer teaser**

In `internal/tui/list_pane.go` line 680, add `i details` to the lean footer teaser (keep it short enough for the narrow pane):

```go
	footer := stMuted.Render("enter open · n new · o dir · s send · a attach · i info · x kill · ? help · q quit")
```

- [ ] **Step 3: Build to confirm no breakage**

Run: `go build ./internal/tui/`
Expected: no output (success).

- [ ] **Step 4: Commit**

```bash
git add internal/tui/view.go internal/tui/list_pane.go
git commit -m "docs(tui): help + footer for i details and p approvals"
```

---

### Task 6: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the full tui suite**

Run: `go test ./internal/tui/...`
Expected: `ok  github.com/srjn45/warden/internal/tui` — all pass.

- [ ] **Step 2: Build the whole module**

Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 3: Run go vet on the touched package**

Run: `go vet ./internal/tui/`
Expected: no output.

- [ ] **Step 4: Manual smoke (left for user — note in handoff)**

The cockpit requires a running daemon + tmux and `make install`/daemon restart to exercise live. After the user rebuilds: open the cockpit (`warden` TUI), confirm (a) rows show only ID/status/context/age and never clip on a narrow pane, (b) `i` on an agent opens the scrollable detail overlay and `i`/`esc` closes it, (c) `p` with `WARDEN_APPROVALS` unset shows the disabled status, and with it set-but-empty shows "no approvals pending", and with a real supervised prompt opens the answerable overlay. Record this as LEFT FOR USER in the memory note.

---

## Notes for the implementer

- No daemon/client/API/web/CLI changes. Everything is in `internal/tui`.
- `max(...)` is already used in `list.go` (line 513) — it's available in this Go version's builtins or a local helper; reuse the same call style already present in the file.
- Do not touch the `enter`-on-⏳-row approvals path (`list_pane.go:532-539`) — it stays as-is; only the standalone key binding moves from `i` to `p`.
- Keep the `modeDetails` rendering identical in shape to `modeDigest` so future maintainers see one pattern.
