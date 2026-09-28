package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
			ExecutionMode:  planstore.PlanModeAutopilot,
			AutopilotRunID: "run-42",
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

	// Plans header always collapsed by default. Items structure:
	// 0: Project Header (expanded — has children)
	// 1: Plans Header (collapsed — always collapsed by default)
	// 2: Agent "agent-1" (BELOW Plans!)
	require.Len(t, items, 3)

	// 0: Project header
	require.NotNil(t, items[0].projHdr)
	require.Equal(t, "My Project", items[0].projHdr.name)

	// 1: Plans header (depth 1, above agents) — always collapsed by default
	require.True(t, items[1].planHeader)
	require.Equal(t, "proj-1", items[1].planProject)
	require.True(t, items[1].collapsed, "plans header is always collapsed by default")

	// 2: Agent row (must appear AFTER/BELOW plans!)
	require.NotNil(t, items[2].session)
	require.Equal(t, "agent-1", items[2].session.ID)

	// Expand plans header and in_progress + archived groups explicitly
	collapsed := map[string]bool{
		"plans:proj-1":             false,
		"plans:proj-1:in_progress": false,
		"plans:proj-1:archived":    false,
	}
	itemsExpanded := buildProjectItems(projs, nil, sessions, nil, client.AutopilotStatus{}, plansMap, nil, collapsed, false)
	// 0=projHdr, 1=planHdr, 2=in_progress(exp), 3=Active Work, 4=pending(coll),
	// 5=completed(coll), 6=archived(exp), 7=Old Plan, 8=agent-1
	require.Len(t, itemsExpanded, 9)
	require.NotNil(t, itemsExpanded[3].plan)
	require.Equal(t, "Active Work", itemsExpanded[3].plan.Name)
	require.NotNil(t, itemsExpanded[7].plan)
	require.Equal(t, "Old Plan", itemsExpanded[7].plan.Name)
	require.NotNil(t, itemsExpanded[8].session)
	require.Equal(t, "agent-1", itemsExpanded[8].session.ID)
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

	// When plans header is collapsed, only projHdr, planHeader, and agent-1 are present
	require.Len(t, items, 3)
	require.NotNil(t, items[0].projHdr)
	require.True(t, items[1].planHeader)
	require.True(t, items[1].collapsed)
	require.NotNil(t, items[2].session)
	require.Equal(t, "agent-1", items[2].session.ID)

	out := renderList(items, 1, 100, 10)
	require.Contains(t, out, "Plans")
	require.NotContains(t, out, "In Progress")
	require.Contains(t, out, "agent-1")
}

func TestPlanTree_EmptyPlansCollapsedByDefault(t *testing.T) {
	projs := []projectstore.Project{
		{ID: "proj-1", Name: "Alpha", Path: "/alpha", Status: projectstore.StatusOpen},
	}
	// Project with 0 plans
	plansMap := map[string][]*planstore.Plan{"proj-1": {}}

	// Project has no children → collapsed by default.
	items := buildProjectItems(projs, nil, nil, nil, client.AutopilotStatus{}, plansMap, nil, nil, false)
	require.Len(t, items, 1)
	require.NotNil(t, items[0].projHdr)
	require.True(t, items[0].collapsed, "project with no children is collapsed by default")

	// With project explicitly expanded, plans header is still collapsed by default.
	expanded := map[string]bool{"project:proj-1": false}
	items2 := buildProjectItems(projs, nil, nil, nil, client.AutopilotStatus{}, plansMap, nil, expanded, false)
	require.Len(t, items2, 3) // projHdr, planHeader(collapsed), empty placeholder
	require.True(t, items2[1].planHeader)
	require.True(t, items2[1].collapsed, "plans header is always collapsed by default")
}

func TestPlanTree_PlanDetailText(t *testing.T) {
	plans := samplePlans()
	p := plans[0] // plan-ip

	text := planDetailText(p, 80, "", false)
	// Header is the plan name (no "Plan:" prefix)
	require.Contains(t, text, "Active Work")
	require.Contains(t, text, "ID:")
	require.Contains(t, text, "plan-ip")
	require.Contains(t, text, "File:")
	require.Contains(t, text, "plans/in_progress/active.yaml")
	require.Contains(t, text, "Status:")
	require.Contains(t, text, "in_progress")
	require.Contains(t, text, "Executed Using:")
	require.Contains(t, text, "autopilot")
	require.Contains(t, text, "Autopilot Run:")
	require.Contains(t, text, "run-42")
	// task-1 and task-2 come from DB TaskProgress (YAML file won't be found in test env)
	require.Contains(t, text, "task-1")
	require.Contains(t, text, "task-2")
	require.Contains(t, text, "Created At:")
	require.Contains(t, text, "Updated At:")
	require.Contains(t, text, "Started At:")

	// Plan with no task progress and no linked execution
	pEmpty := &planstore.Plan{
		ID:        "p-empty",
		ProjectID: "proj-1",
		Name:      "Empty Plan",
		FilePath:  "plans/pending/empty.yaml",
		Status:    planstore.PlanStatusPending,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	textEmpty := planDetailText(pEmpty, 80, "", false)
	require.Contains(t, textEmpty, "Executed Using:")
	require.Contains(t, textEmpty, "manual")
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

	// Move cursor to active plan (index 3: projHdr, planHdr, in_progress group, active plan)
	m.cursor = 3
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

	// Test 's' on plan row (cursor 3)
	m.cursor = 3
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

	m.cursor = 3
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

	m.cursor = 3
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

	// Case 1: Cockpit with agentPane ("%9")
	m := setupPlanTestModel(a)
	m.agentPane = "%9"
	m.cursor = 3
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)

	nm, cmd := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Equal(t, "plan-ip", m.openedPlan)
	require.NotNil(t, cmd)

	// Case 2: Cockpit without agentPane (in-pane detail)
	mNoAgent := setupPlanTestModel(a)
	mNoAgent.agentPane = ""
	mNoAgent.cursor = 3

	nmNoAgent, _ := mNoAgent.Update(key("enter"))
	mNoAgent = nmNoAgent.(controlPaneModel)
	require.Equal(t, modePlanDetail, mNoAgent.mode)
	require.Equal(t, "plan-ip", mNoAgent.targetPlanID)
	require.Contains(t, mNoAgent.vp.View(), "Active Work")

	// Esc returns to normal
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

	// Enter on status group (cursor 2: in_progress) — group starts with a pre-set
	// value from setupPlanTestModel (false = expanded), so toggling collapses it.
	m.cursor = 2
	require.Equal(t, "in_progress", itemAt(m.items(), m.cursor).planGroup)
	m = lstep(m, key("enter"))
	require.True(t, m.collapsed["plans:proj-1:in_progress"], "toggle collapses an expanded group")

	// Re-expand in_progress so the plan row at cursor 3 is visible
	m.collapsed["plans:proj-1:in_progress"] = false
	m.cursor = 3 // Active Work plan (under in_progress group)
	require.NotNil(t, itemAt(m.items(), m.cursor).plan)
	m = lstep(m, key("h"))
	require.True(t, m.collapsed["plans:proj-1:in_progress"], "h/left on plan row collapses its group")
}
