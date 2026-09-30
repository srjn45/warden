package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func samplePlans() []*planstore.Plan {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	started := now.Add(10 * time.Minute)
	completed := now.Add(30 * time.Minute)
	return []*planstore.Plan{
		{
			ID:             "plan-ip",
			ProjectID:      "proj-1",
			Name:           "Active Work",
			FilePath:       "plans/in_progress/active.yaml",
			Status:         planstore.PlanStatusInProgress,
			Revision:       3,
			ExecutionMode:  planstore.PlanModeAutopilot,
			AutopilotRunID: "run-42",
			Tasks: []planstore.PlanTask{
				{ID: "task-1", Prompt: "first"},
				{ID: "task-2", Prompt: "second"},
			},
			TaskProgress: map[string]string{
				"task-1": "done",
				"task-2": "in_progress",
			},
			CreatedAt: now,
			UpdatedAt: now.Add(5 * time.Minute),
			StartedAt: &started,
		},
		{
			ID:            "plan-p1",
			ProjectID:     "proj-1",
			Name:          "Pending Feature A",
			FilePath:      "plans/pending/feat-a.yaml",
			Status:        planstore.PlanStatusPending,
			ExecutionMode: planstore.PlanModePipeline,
			PipelineID:    "pipe-99",
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:            "plan-p2",
			ProjectID:     "proj-1",
			Name:          "Pending Feature B",
			FilePath:      "plans/pending/feat-b.yaml",
			Status:        planstore.PlanStatusPending,
			ExecutionMode: planstore.PlanModeManual,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		{
			ID:             "plan-c1",
			ProjectID:      "proj-1",
			Name:           "Done Feature",
			FilePath:       "plans/completed/done.yaml",
			Status:         planstore.PlanStatusCompleted,
			ExecutionMode:  planstore.PlanModeOrchestratorWorker,
			OrchestratorID: "orch-7",
			CreatedAt:      now,
			UpdatedAt:      completed,
			StartedAt:      &started,
			CompletedAt:    &completed,
		},
		{
			ID:            "plan-a1",
			ProjectID:     "proj-1",
			Name:          "Old Plan",
			FilePath:      "plans/archived/old.yaml",
			Status:        planstore.PlanStatusArchived,
			ExecutionMode: planstore.PlanModeManual,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
	}
}

func TestPlanTree_StructureAndGrouping(t *testing.T) {
	projs := []projectstore.Project{
		{ID: "proj-1", Name: "My Project", Path: "/my/project", Status: projectstore.StatusOpen},
	}
	plans := samplePlans()
	plansMap := map[string][]*planstore.Plan{"proj-1": plans}
	sessions := []*store.Session{
		{ID: "agent-1", ProjectID: "proj-1", Workdir: "/my/project", Status: store.StatusWorking},
	}

	items := buildProjectItems(projs, nil, sessions, nil, client.AutopilotStatus{}, plansMap, nil, nil, false)

	// Project sections: Plans (collapsed); Autopilots/Pipelines/Agents/Terminals open by default.
	require.NotNil(t, items[0].projHdr)
	require.Equal(t, "My Project", items[0].projHdr.name)
	require.True(t, items[1].planHeader)
	require.True(t, items[1].collapsed, "plans header is always collapsed by default")
	require.Equal(t, "Autopilots", items[2].treeSecLabel)
	require.False(t, items[2].collapsed, "Autopilots open by default")
	require.Equal(t, "Pipelines", items[3].treeSecLabel)
	require.False(t, items[3].collapsed, "Pipelines open by default")
	require.Equal(t, "Agents", items[4].treeSecLabel)
	require.False(t, items[4].collapsed)
	require.NotNil(t, items[5].session)
	require.Equal(t, "agent-1", items[5].session.ID)
	require.Equal(t, "Terminals", items[6].treeSecLabel)

	// Expand plans header and in_progress + archived groups explicitly
	collapsed := map[string]bool{
		"plans:proj-1":             false,
		"plans:proj-1:in_progress": false,
		"plans:proj-1:archived":    false,
		"section:proj-1:agents":    false,
	}
	itemsExpanded := buildProjectItems(projs, nil, sessions, nil, client.AutopilotStatus{}, plansMap, nil, collapsed, false)
	var planNames []string
	var sawAgent bool
	for _, it := range itemsExpanded {
		if it.plan != nil {
			planNames = append(planNames, it.plan.Name)
		}
		if it.session != nil && it.session.ID == "agent-1" {
			sawAgent = true
		}
	}
	require.Contains(t, planNames, "Active Work")
	require.Contains(t, planNames, "Old Plan")
	require.True(t, sawAgent, "agent must appear below Plans section")
}

func TestPlanTree_BadgesAndRendering(t *testing.T) {
	projs := []projectstore.Project{
		{ID: "proj-1", Name: "Alpha", Path: "/alpha", Status: projectstore.StatusOpen},
	}
	plans := samplePlans()
	plansMap := map[string][]*planstore.Plan{"proj-1": plans}

	// Project has no sessions → collapsed by default; plans header also collapsed by default.
	// Explicitly open both so we can verify badges in the rendered output.
	collapsedMap := map[string]bool{"project:proj-1": false, "plans:proj-1": false}
	items := buildProjectItems(projs, nil, nil, nil, client.AutopilotStatus{}, plansMap, nil, collapsedMap, false)
	out := renderList(items, 1, 100, 20)

	// Plans header
	require.Contains(t, out, "Plans")

	// Badges
	require.Contains(t, out, "In Progress  (1)")
	require.Contains(t, out, "Pending  (2)")
	require.Contains(t, out, "Completed  (1)")

	// Archived has NO count badge (spec D14: "Archived is collapsed by default, no badge")
	require.Contains(t, out, "Archived")
	require.NotContains(t, out, "Archived  (1)")

	// Plan items are not visible when groups are collapsed by default
	require.NotContains(t, out, "Active Work", "plan items hidden when group is collapsed")
	require.NotContains(t, out, "Old Plan", "archived plan hidden when group is collapsed")
}

func TestPlanTree_CollapsePlansHeader(t *testing.T) {
	projs := []projectstore.Project{
		{ID: "proj-1", Name: "Alpha", Path: "/alpha", Status: projectstore.StatusOpen},
	}
	plans := samplePlans()
	plansMap := map[string][]*planstore.Plan{"proj-1": plans}
	sessions := []*store.Session{
		{ID: "agent-1", ProjectID: "proj-1", Workdir: "/alpha", Status: store.StatusWorking},
	}

	collapsed := map[string]bool{"plans:proj-1": true}
	items := buildProjectItems(projs, nil, sessions, nil, client.AutopilotStatus{}, plansMap, nil, collapsed, false)

	require.NotNil(t, items[0].projHdr)
	require.True(t, items[1].planHeader)
	require.True(t, items[1].collapsed)
	var sawAgent bool
	for _, it := range items {
		if it.session != nil && it.session.ID == "agent-1" {
			sawAgent = true
		}
		require.Nil(t, it.plan, "collapsed Plans must hide plan rows")
	}
	require.True(t, sawAgent)

	out := renderList(items, 1, 100, 10)
	require.Contains(t, out, "Plans")
	require.NotContains(t, out, "In Progress")
	require.Contains(t, out, "agent-1")
}

func TestPlanTree_EmptyPlansCollapsedByDefault(t *testing.T) {
	projs := []projectstore.Project{
		{ID: "proj-1", Name: "Alpha", Path: "/alpha", Status: projectstore.StatusOpen},
	}
	// Project with 0 plans — still has five empty sections, so the project expands.
	plansMap := map[string][]*planstore.Plan{"proj-1": {}}

	items := buildProjectItems(projs, nil, nil, nil, client.AutopilotStatus{}, plansMap, nil, nil, false)
	require.GreaterOrEqual(t, len(items), 1)
	require.NotNil(t, items[0].projHdr)
	// Sections are non-empty structurally (5 section headers), so project is open.
	require.False(t, items[0].collapsed, "project with section children is expanded")
	require.True(t, items[1].planHeader)
	require.True(t, items[1].collapsed, "plans header is always collapsed by default")

	// With plans header explicitly expanded, status groups appear (all empty/collapsed).
	collapsed := map[string]bool{"project:proj-1": false, "plans:proj-1": false}
	itemsOpen := buildProjectItems(projs, nil, nil, nil, client.AutopilotStatus{}, plansMap, nil, collapsed, false)
	var sawGroup bool
	for _, it := range itemsOpen {
		if it.planGroup != "" {
			sawGroup = true
			break
		}
	}
	require.True(t, sawGroup)
}

func TestPlanTree_PlanDetailText(t *testing.T) {
	plans := samplePlans()
	p := plans[0] // plan-ip

	text := planDetailText(p, 80, "", false)
	// Header is the plan name (no "Plan:" prefix)
	require.Contains(t, text, "Active Work")
	require.Contains(t, text, "ID:")
	require.Contains(t, text, "plan-ip")
	require.Contains(t, text, "Revision:")
	require.Contains(t, text, "Export:")
	require.Contains(t, text, "Last Export:")
	require.Contains(t, text, "plans/in_progress/active.yaml")
	require.NotContains(t, text, "File:")
	require.Contains(t, text, "Status:")
	require.Contains(t, text, "in_progress")
	require.Contains(t, text, "Executed Using:")
	require.Contains(t, text, "autopilot")
	require.Contains(t, text, "Executor:")
	require.Contains(t, text, "run-42")
	require.Contains(t, text, "Autopilot Run:")
	require.Contains(t, text, "Tasks:")
	require.Contains(t, text, "1/2 done")
	// task-1 and task-2 come from canonical Tasks (no YAML read)
	require.Contains(t, text, "task-1")
	require.Contains(t, text, "task-2")
	require.Contains(t, text, "Created At:")
	require.Contains(t, text, "Updated At:")
	require.Contains(t, text, "Started At:")
	require.Contains(t, text, "Lifecycle")
	require.Contains(t, text, "Active Execution")
	require.Contains(t, text, "Task Evidence")
	require.Contains(t, text, "Historical Summaries")
	require.Contains(t, text, "[↑↓] scroll")

	// Plan with no tasks and no linked execution — empty FilePath still works.
	pEmpty := &planstore.Plan{
		ID:        "p-empty",
		ProjectID: "proj-1",
		Name:      "Empty Plan",
		Status:    planstore.PlanStatusPending,
		Revision:  1,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	textEmpty := planDetailText(pEmpty, 80, "", false)
	require.Contains(t, textEmpty, "Executed Using:")
	require.Contains(t, textEmpty, "manual")
	require.Contains(t, textEmpty, "Export:")
	require.Contains(t, textEmpty, "none")
	require.Contains(t, textEmpty, "(no active execution)")
	require.Contains(t, textEmpty, "(no tasks defined)")
	require.Contains(t, textEmpty, "(no historical summaries yet)")
}

func TestPlanDetailText_NoExportIdenticalToExportMetaAbsent(t *testing.T) {
	now := time.Now()
	p := &planstore.Plan{
		ID: "plan-db-only", ProjectID: "proj-1", Name: "DB Only",
		Status: planstore.PlanStatusPending, Revision: 2,
		Goal: "works without plans/", Tasks: []planstore.PlanTask{{ID: "t1", Prompt: "go"}},
		CreatedAt: now, UpdatedAt: now,
	}
	text := planDetailText(p, 80, "/nonexistent", true)
	require.Contains(t, text, "t1")
	require.Contains(t, text, "go")
	require.Contains(t, text, "Export:")
	require.Contains(t, text, "none")
	require.NotContains(t, text, "Last Export:")
	require.Contains(t, text, "[t] toggle task details")
}

func setupPlanTestModel(a *fakeAPI) controlPaneModel {
	m := newListPane(a, "%9", "")
	m.w = 100
	m.h = 40
	m.vp.Width = 96
	m.vp.Height = 38
	m.ready = true
	m.projects = []projectstore.Project{
		{ID: "proj-1", Name: "Alpha", Path: "/alpha", Status: projectstore.StatusOpen},
	}
	m.plans = map[string][]*planstore.Plan{
		"proj-1": samplePlans(),
	}
	// Pre-expand project and plans header so keybinding tests can navigate into plans.
	// With new defaults, both are collapsed until explicitly opened.
	m.collapsed["project:proj-1"] = false
	m.collapsed["plans:proj-1"] = false
	m.collapsed["plans:proj-1:in_progress"] = false
	return m
}

func TestPlanKeybindings_Archive(t *testing.T) {
	a := &fakeAPI{}
	m := setupPlanTestModel(a)

	// Move cursor to active plan (index 4: projHdr, planHdr, pending(coll), in_progress group, active plan)
	m.cursor = 4
	it := itemAt(m.items(), m.cursor)
	require.NotNil(t, it.plan)
	require.Equal(t, "plan-ip", it.plan.ID)

	// Press 'a' to archive
	nm, cmd := m.Update(key("a"))
	m = nm.(controlPaneModel)
	require.Equal(t, "archiving plan plan-ip…", m.status)
	require.NotNil(t, cmd)

	msg := cmd().(planArchivedMsg)
	require.Equal(t, "proj-1", msg.projectID)
	require.Equal(t, "plan-ip", msg.planID)

	// Handle completion message
	nm, refreshCmd := m.Update(msg)
	m = nm.(controlPaneModel)
	require.Equal(t, "archived plan plan-ip", m.status)
	require.NotNil(t, refreshCmd)
}

func TestPlanKeybindings_Scan(t *testing.T) {
	a := &fakeAPI{
		planScanRes: client.PlanScanResult{Upserted: 3},
	}
	m := setupPlanTestModel(a)

	// Test 's' on plan header (cursor 1)
	m.cursor = 1
	require.True(t, itemAt(m.items(), m.cursor).planHeader)
	nm, cmd := m.Update(key("s"))
	m = nm.(controlPaneModel)
	require.Equal(t, "scanning plans in proj-1…", m.status)
	require.NotNil(t, cmd)

	msg := cmd().(plansScannedMsg)
	require.Equal(t, "proj-1", msg.projectID)
	require.Equal(t, 3, msg.res.Upserted)

	nm, refreshCmd := m.Update(msg)
	m = nm.(controlPaneModel)
	require.Equal(t, "scanned 3 plans", m.status)
	require.NotNil(t, refreshCmd)

	// Test 's' on plan group (cursor 2)
	m.cursor = 2
	require.NotEmpty(t, itemAt(m.items(), m.cursor).planGroup)
	nm, cmdGroup := m.Update(key("s"))
	m = nm.(controlPaneModel)
	require.Equal(t, "scanning plans in proj-1…", m.status)
	require.NotNil(t, cmdGroup)

	// Test 's' on plan row (cursor 4)
	m.cursor = 4
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)
	nm, cmdPlan := m.Update(key("s"))
	m = nm.(controlPaneModel)
	require.Equal(t, "scanning plans in proj-1…", m.status)
	require.NotNil(t, cmdPlan)
}

func TestPlanKeybindings_Assess(t *testing.T) {
	a := &fakeAPI{
		planAssessPlan: &planstore.Plan{
			ID:        "plan-ip",
			ProjectID: "proj-1",
			Status:    planstore.PlanStatusInProgress,
		},
	}
	m := setupPlanTestModel(a)

	m.cursor = 4
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)

	// Press 'A' to assess
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
	m = nm.(controlPaneModel)
	require.Equal(t, "assessing plan plan-ip (brain)…", m.status)
	require.NotNil(t, cmd)

	msg := cmd().(planAssessedMsg)
	require.Equal(t, "proj-1", msg.projectID)
	require.Equal(t, "plan-ip", msg.planID)

	nm, refreshCmd := m.Update(msg)
	m = nm.(controlPaneModel)
	require.Equal(t, "assessed plan plan-ip", m.status)
	require.NotNil(t, refreshCmd)
}

func TestPlanKeybindings_RunModePicker(t *testing.T) {
	a := &fakeAPI{}
	m := setupPlanTestModel(a)

	m.cursor = 4
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)

	// Press 'r' to open mode picker
	m = lstep(m, key("r"))
	require.Equal(t, modePlanRunMode, m.mode)
	require.Equal(t, "plan-ip", m.targetPlanID)
	require.Equal(t, 0, m.planRunModeIdx) // autopilot

	// Cycle forward with 'l' or 'right'
	m = lstep(m, key("l"))
	require.Equal(t, 1, m.planRunModeIdx) // pipeline

	m = lstep(m, key("l"))
	require.Equal(t, 2, m.planRunModeIdx) // orchestrator_worker

	m = lstep(m, key("l"))
	require.Equal(t, 3, m.planRunModeIdx) // manual

	m = lstep(m, key("l"))
	require.Equal(t, 0, m.planRunModeIdx) // wraps back to autopilot

	// Cycle backward with 'h'
	m = lstep(m, key("h"))
	require.Equal(t, 3, m.planRunModeIdx) // manual

	// Press Enter to select mode
	nm, cmd := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Equal(t, modeNormal, m.mode)
	require.Contains(t, m.status, "running plan plan-ip in manual mode…")
	require.NotNil(t, cmd)

	msg := cmd().(planRunMsg)
	require.Equal(t, "manual", msg.mode)
	require.Equal(t, "plan-ip", msg.planID)

	// Test Esc cancels mode picker
	m.mode = modePlanRunMode
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestPlanKeybindings_EnterDetail(t *testing.T) {
	a := &fakeAPI{}

	// Case 1: Cockpit with agentPane still uses the in-control-pane viewport.
	m := setupPlanTestModel(a)
	m.agentPane = "%9"
	m.cursor = -1
	for i, it := range m.items() {
		if it.plan != nil && it.plan.ID == "plan-ip" {
			m.cursor = i
			break
		}
	}
	require.GreaterOrEqual(t, m.cursor, 0)
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)

	nm, cmd := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Equal(t, "plan-ip", m.openedPlan)
	require.Nil(t, cmd)
	require.Equal(t, modePlanDetail, m.mode)
	require.Equal(t, "plan-ip", m.targetPlanID)
	require.Contains(t, m.vp.View(), "Active Work")

	nm, toggleCmd := m.Update(key("t"))
	m = nm.(controlPaneModel)
	require.Nil(t, toggleCmd)
	require.True(t, m.planDetailExpanded)

	// Case 2: Cockpit without agentPane also uses the in-pane detail.
	mNoAgent := setupPlanTestModel(a)
	mNoAgent.agentPane = ""
	mNoAgent.cursor = -1
	for i, it := range mNoAgent.items() {
		if it.plan != nil && it.plan.ID == "plan-ip" {
			mNoAgent.cursor = i
			break
		}
	}
	require.GreaterOrEqual(t, mNoAgent.cursor, 0)
	mNoAgent = lstep(mNoAgent, key("enter"))
	require.Equal(t, modePlanDetail, mNoAgent.mode)
	require.Contains(t, mNoAgent.vp.View(), "Active Work")

	mNoAgent.mode = modePlanDetail
	mNoAgent = lstep(mNoAgent, key("esc"))
	require.Equal(t, modeNormal, mNoAgent.mode)

	// 'q' also returns to normal from detail
	mNoAgent.mode = modePlanDetail
	mNoAgent = lstep(mNoAgent, key("q"))
	require.Equal(t, modeNormal, mNoAgent.mode)
}

func TestPlanKeybindings_ToggleHeaders(t *testing.T) {
	a := &fakeAPI{}
	m := setupPlanTestModel(a)

	// Enter on Plans header (cursor 1) collapses it
	m.cursor = 1
	require.True(t, itemAt(m.items(), m.cursor).planHeader)
	m = lstep(m, key("enter"))
	require.True(t, m.collapsed["plans:proj-1"])

	// Enter again expands it
	m = lstep(m, key("enter"))
	require.False(t, m.collapsed["plans:proj-1"])

	// Enter on status group (cursor 3: in_progress) — group starts with a pre-set
	// value from setupPlanTestModel (false = expanded), so toggling collapses it.
	m.cursor = 3
	require.Equal(t, "in_progress", itemAt(m.items(), m.cursor).planGroup)
	m = lstep(m, key("enter"))
	require.True(t, m.collapsed["plans:proj-1:in_progress"], "toggle collapses an expanded group")

	// Re-expand in_progress so the plan row at cursor 4 is visible
	m.collapsed["plans:proj-1:in_progress"] = false
	m.cursor = 4 // Active Work plan (under in_progress group)
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)
	m = lstep(m, key("h"))
	require.True(t, m.collapsed["plans:proj-1:in_progress"], "h/left on plan row collapses its group")
}

func TestPlanDetail_ArrowKeysScrollViewport(t *testing.T) {
	a := &fakeAPI{}
	m := setupPlanTestModel(a)
	m.cursor = -1
	for i, it := range m.items() {
		if it.plan != nil && it.plan.ID == "plan-ip" {
			m.cursor = i
			break
		}
	}
	require.GreaterOrEqual(t, m.cursor, 0, "Active Work plan row must be visible")
	m = lstep(m, key("enter"))
	require.Equal(t, modePlanDetail, m.mode)

	// Tall content so ↑/↓ can move the viewport (modePlanDetail forwards to m.vp.Update).
	var lines []string
	for i := 0; i < 80; i++ {
		lines = append(lines, fmt.Sprintf("line-%02d", i))
	}
	m.vp.Height = 10
	m.vp.SetContent(strings.Join(lines, "\n"))
	require.Equal(t, 0, m.vp.YOffset)

	m = lstep(m, key("down"))
	require.Greater(t, m.vp.YOffset, 0, "↓ should scroll the plan detail viewport")
	scrolled := m.vp.YOffset

	m = lstep(m, key("up"))
	require.Less(t, m.vp.YOffset, scrolled, "↑ should scroll the plan detail viewport up")
}

func TestPromptPreview_WrapsAtWidth(t *testing.T) {
	long := strings.Repeat("word ", 30)
	lines := promptPreview(long, 20, 20)
	require.Greater(t, len(lines), 1, "long prompt should wrap into multiple display lines")
	for _, line := range lines {
		require.LessOrEqual(t, lipgloss.Width(line), 20, "wrapped line exceeds width: %q", line)
	}

	// maxLines caps display lines after wrapping.
	capped := promptPreview(long, 2, 20)
	require.Len(t, capped, 2)

	// width <= 0 disables wrapping.
	nowrap := promptPreview(long, 1, 0)
	require.Len(t, nowrap, 1)
	require.Equal(t, strings.TrimRight(long, " \t"), nowrap[0])
}

func TestPlanDetailText_ScrollHintAndPromptWrap(t *testing.T) {
	longPrompt := strings.Repeat("abcdefghij ", 20) // ~220 chars
	p := &planstore.Plan{
		ID:        "p-wrap",
		ProjectID: "proj-1",
		Name:      "Wrap Plan",
		Status:    planstore.PlanStatusPending,
		Revision:  1,
		Tasks:     []planstore.PlanTask{{ID: "t1", Prompt: longPrompt}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	const width = 40
	text := planDetailText(p, width, "", true)
	require.Contains(t, text, "[↑↓] scroll")
	require.Contains(t, text, "[t] toggle task details")

	wrapW := width - lipgloss.Width(promptIndent)
	preview := promptPreview(longPrompt, 3, wrapW)
	require.Greater(t, len(preview), 1)
	for _, line := range preview {
		require.LessOrEqual(t, lipgloss.Width(line), wrapW)
	}
	// Expanded detail should include the first wrapped fragment.
	require.Contains(t, text, preview[0])
}
