package tree

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

func sectionOf(t *testing.T, project *Node, kind SectionKind) *Node {
	t.Helper()
	require.NotNil(t, project)
	for _, ch := range project.Children {
		if ch != nil && ch.Type == NodeTypeSection && ch.Detail != nil && ch.Detail.Section == string(kind) {
			return ch
		}
	}
	t.Fatalf("section %q not found under %s", kind, project.ID)
	return nil
}

func hasSection(project *Node, kind SectionKind) bool {
	if project == nil {
		return false
	}
	for _, ch := range project.Children {
		if ch != nil && ch.Type == NodeTypeSection && ch.Detail != nil && ch.Detail.Section == string(kind) {
			return true
		}
	}
	return false
}

// projectEntities returns direct (non-section) children of the given type.
func projectEntities(project *Node, typ NodeType) []*Node {
	if project == nil {
		return nil
	}
	var out []*Node
	for _, ch := range project.Children {
		if ch != nil && ch.Type == typ {
			out = append(out, ch)
		}
	}
	return out
}

// requireCanonicalProjectOrder asserts project children follow
// Plans (section) → Autopilot runs → Pipelines → Agents → Terminals (section)
// with no Autopilots/Pipelines/Agents section buckets.
func requireCanonicalProjectOrder(t *testing.T, project *Node) {
	t.Helper()
	require.NotNil(t, project)
	prev := -1
	for _, ch := range project.Children {
		require.NotNil(t, ch)
		rank := -1
		switch {
		case ch.Type == NodeTypeSection && ch.Detail != nil && ch.Detail.Section == string(SectionPlans):
			rank = 0
			require.NotEmpty(t, ch.Children, "Plans section must not be empty")
		case ch.Type == NodeTypeAutopilotRun:
			rank = 1
		case ch.Type == NodeTypePipeline:
			rank = 2
		case ch.Type == NodeTypeAgent:
			rank = 3
		case ch.Type == NodeTypeSection && ch.Detail != nil && ch.Detail.Section == string(SectionTerminals):
			rank = 4
			require.NotEmpty(t, ch.Children, "Terminals section must not be empty")
		default:
			t.Fatalf("unexpected project child type=%q id=%q section=%v", ch.Type, ch.ID, ch.Detail)
		}
		require.GreaterOrEqual(t, rank, prev, "project children out of order at %q", ch.ID)
		prev = rank
	}
	require.False(t, hasSection(project, SectionAutopilots), "Autopilots section bucket must not exist")
	require.False(t, hasSection(project, SectionPipelines), "Pipelines section bucket must not exist")
	require.False(t, hasSection(project, SectionAgents), "Agents section bucket must not exist")
}

func TestProjectSections_CanonicalOrder(t *testing.T) {
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Agents: []string{}, Pipelines: []string{}, Terminals: []string{},
			Plans: []string{}, Autopilots: []string{},
		}},
	}
	tr := NewService().Build(in, "")
	require.Len(t, tr.Roots, 1)
	requireCanonicalProjectOrder(t, tr.Roots[0])
	require.Empty(t, tr.Roots[0].Children, "fully empty project has no children")
}

func TestProjectSections_OmitEmptyPlansAndTerminals(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Plans: []string{"plan-1"}, Agents: []string{"a1"},
		}},
		Plans: []*planstore.Plan{{
			ID: "plan-1", ProjectID: "/p", Name: "feat", Status: planstore.PlanStatusInProgress,
		}},
		Sessions: []*store.Session{
			{ID: "a1", Name: "solo", ProjectID: "/p", Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	tr := NewService().Build(in, "")
	proj := tr.Roots[0]
	requireCanonicalProjectOrder(t, proj)
	require.True(t, hasSection(proj, SectionPlans))
	require.Len(t, projectEntities(proj, NodeTypeAgent), 1)
	require.False(t, hasSection(proj, SectionTerminals))
	require.Len(t, proj.Children, 2) // Plans section + agent
}

func TestProjectSections_OmitEmptyPlans(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Agents: []string{"a1"},
		}},
		Sessions: []*store.Session{
			{ID: "a1", Name: "solo", ProjectID: "/p", Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	tr := NewService().Build(in, "")
	proj := tr.Roots[0]
	requireCanonicalProjectOrder(t, proj)
	require.False(t, hasSection(proj, SectionPlans))
	require.Len(t, projectEntities(proj, NodeTypeAgent), 1)
	require.Len(t, proj.Children, 1)
}

func TestExactlyOnce_LiveAutopilotManagerWorkersNotAlsoAgents(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Agents: []string{"mgr", "worker-1", "O:orch"}, Pipelines: []string{},
			Terminals: []string{}, Plans: []string{"plan-1"}, Autopilots: []string{"ap-1"},
		}},
		Plans: []*planstore.Plan{{
			ID: "plan-1", ProjectID: "/p", Name: "feat", Status: planstore.PlanStatusInProgress,
			ExecutionMode: planstore.PlanModeAutopilot, AutopilotRunID: "ap-1",
		}},
		Autopilots: []*autopilotstore.Autopilot{{
			ID: "ap-1", ProjectID: "/p", PlanID: "plan-1", Name: "AP:feat",
			ManagerAgentID: "mgr", BrainAgentID: "brain-1",
			Diagnostics: autopilotstore.Diagnostics{State: "active", Repo: "/p", Gate: "local"},
		}},
		Sessions: []*store.Session{
			{ID: "mgr", Name: "AP:feat", Role: "autopilot", PlanID: "plan-1", ProjectID: "/p",
				ChildAgents: []string{"worker-1"}, Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now},
			{ID: "worker-1", Name: "w1", Role: "worker", ParentID: "mgr", PlanID: "plan-1", ProjectID: "/p",
				Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now.Add(time.Minute)},
			{ID: "brain-1", Name: "brain", Role: "brain", PlanID: "plan-1", ProjectID: "/p",
				Tags: []string{"system:true", "run:ap-1"}, Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
			{ID: "O:orch", Name: "O:feat", Role: "orchestrator", PlanID: "plan-other", ProjectID: "/p",
				Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	tr := NewService().Build(in, "")
	proj := tr.Roots[0]
	requireCanonicalProjectOrder(t, proj)

	plans := sectionOf(t, proj, SectionPlans)
	require.Len(t, plans.Children, 1)
	require.Equal(t, "plan:plan-1", plans.Children[0].ID)
	require.Empty(t, plans.Children[0].Children, "Plan nodes must not nest executors")

	aps := projectEntities(proj, NodeTypeAutopilotRun)
	require.Len(t, aps, 1)
	run := aps[0]
	require.Equal(t, "AP:feat", run.Label)
	require.Len(t, run.Children, 1, "manager only; no task groups, no brain")
	mgr := run.Children[0]
	require.Equal(t, NodeTypeManager, mgr.Type)
	require.Equal(t, "session:mgr", mgr.ID)
	require.Len(t, mgr.Children, 1)
	require.Equal(t, "session:worker-1", mgr.Children[0].ID)
	require.Equal(t, NodeTypeWorker, mgr.Children[0].Type)

	// No task nodes anywhere under Autopilot.
	var walk func([]*Node)
	walk = func(ns []*Node) {
		for _, n := range ns {
			require.NotEqual(t, NodeTypeTask, n.Type, "Plan task groups must not render inside Autopilot")
			walk(n.Children)
		}
	}
	walk(aps)

	agents := projectEntities(proj, NodeTypeAgent)
	require.Len(t, agents, 1, "manager+worker claimed by Autopilot; only O: remains")
	require.Equal(t, "session:O:orch", agents[0].ID)
}

func TestExactlyOnce_HideBrainUnlessShowSystem(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Autopilots: []string{"ap-1"}, Agents: []string{},
		}},
		Autopilots: []*autopilotstore.Autopilot{{
			ID: "ap-1", ProjectID: "/p", PlanID: "plan-1", Name: "AP:x",
			ManagerAgentID: "mgr", BrainAgentID: "brain-1",
			Diagnostics: autopilotstore.Diagnostics{State: "active", Repo: "/p"},
		}},
		Sessions: []*store.Session{
			{ID: "mgr", Role: "autopilot", ProjectID: "/p", Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now},
			{ID: "brain-1", Role: "brain", ProjectID: "/p", Tags: []string{"system:true"},
				Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	hidden := NewService().Build(in, "")
	aps := projectEntities(hidden.Roots[0], NodeTypeAutopilotRun)
	require.Len(t, aps, 1)
	require.Len(t, aps[0].Children, 1)
	require.Equal(t, "session:mgr", aps[0].Children[0].ID)

	in.ShowSystem = true
	shown := NewService().Build(in, "")
	aps2 := projectEntities(shown.Roots[0], NodeTypeAutopilotRun)
	require.GreaterOrEqual(t, len(aps2[0].Children), 2)
	ids := map[string]bool{}
	for _, ch := range aps2[0].Children {
		ids[ch.SessionID] = true
	}
	require.True(t, ids["mgr"])
	require.True(t, ids["brain-1"])
}

func TestExactlyOnce_PipelineJobsNotAgents(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Pipelines: []string{"plan-p"}, Agents: []string{"M:solo"},
		}},
		Pipelines: []*pipeline.Pipeline{{
			ID: "plan-p", Name: "P:feat", ProjectID: "/p", PlanID: "plan-1",
			Status: pipeline.StatusRunning,
			Jobs:   []pipeline.Job{{ID: "t1", Status: pipeline.JobRunning, SessionID: "job-agent"}},
		}},
		Sessions: []*store.Session{
			{ID: "job-agent", PipelineID: "plan-p", JobID: "t1", ProjectID: "/p",
				Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now},
			{ID: "M:solo", Name: "M:feat", Role: "general", PlanID: "plan-2", ProjectID: "/p",
				Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	tr := NewService().Build(in, "")
	pipes := projectEntities(tr.Roots[0], NodeTypePipeline)
	require.Len(t, pipes, 1)
	require.Equal(t, "P:feat", pipes[0].Label)
	require.Equal(t, "pipeline:plan-p/job:t1", pipes[0].Children[0].ID)

	agents := projectEntities(tr.Roots[0], NodeTypeAgent)
	require.Len(t, agents, 1)
	require.Equal(t, "session:M:solo", agents[0].ID)
}

func TestExactlyOnce_OrchestratorWithWorkersUnderAgents(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Agents: []string{"O:feat", "w1"},
		}},
		Sessions: []*store.Session{
			{ID: "O:feat", Name: "O:feat", Role: "orchestrator", PlanID: "plan-1", ProjectID: "/p",
				ChildAgents: []string{"w1"}, Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now},
			{ID: "w1", Name: "worker", Role: "worker", ParentID: "O:feat", PlanID: "plan-1", ProjectID: "/p",
				Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	tr := NewService().Build(in, "")
	agents := projectEntities(tr.Roots[0], NodeTypeAgent)
	require.Len(t, agents, 1)
	require.Equal(t, "O:feat", agents[0].Label)
	require.Len(t, agents[0].Children, 1)
	require.Equal(t, "session:w1", agents[0].Children[0].ID)
	require.Empty(t, projectEntities(tr.Roots[0], NodeTypeAutopilotRun), "no root autopilots")
}

func TestNestedAutopilot_UnderOwningAgent(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Agents: []string{"orch"}, Autopilots: []string{"ap-nested", "ap-root"},
		}},
		Autopilots: []*autopilotstore.Autopilot{
			{
				ID: "ap-nested", ProjectID: "/p", Name: "nested-run", ParentAgentID: "orch",
				ManagerAgentID: "nested-mgr",
				Diagnostics:    autopilotstore.Diagnostics{State: "active", Repo: "/p"},
			},
			{
				ID: "ap-root", ProjectID: "/p", Name: "root-run",
				ManagerAgentID: "root-mgr",
				Diagnostics:    autopilotstore.Diagnostics{State: "active", Repo: "/p"},
			},
		},
		Sessions: []*store.Session{
			{
				ID: "orch", Name: "orchestrator", ProjectID: "/p", Repo: "/p",
				Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now,
				ChildAutopilots: []string{"ap-nested"},
				ChildAgents:     []string{"kid"},
			},
			{
				ID: "kid", Name: "child", ParentID: "orch", ProjectID: "/p", Repo: "/p",
				Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now.Add(time.Minute),
			},
			{
				ID: "nested-mgr", Name: "nested-mgr", Role: "autopilot", ProjectID: "/p",
				Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now,
			},
			{
				ID: "root-mgr", Name: "root-mgr", Role: "autopilot", ProjectID: "/p",
				Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now,
			},
		},
	}
	tr := NewService().Build(in, "")
	proj := tr.Roots[0]
	requireCanonicalProjectOrder(t, proj)

	rootAPs := projectEntities(proj, NodeTypeAutopilotRun)
	require.Len(t, rootAPs, 1)
	require.Equal(t, "run:ap-root", rootAPs[0].ID)

	agents := projectEntities(proj, NodeTypeAgent)
	require.Len(t, agents, 1)
	orch := agents[0]
	require.Equal(t, "session:orch", orch.ID)
	require.GreaterOrEqual(t, len(orch.Children), 2)
	require.Equal(t, NodeTypeAutopilotRun, orch.Children[0].Type, "child autopilots before child agents")
	require.Equal(t, "run:ap-nested", orch.Children[0].ID)
	require.Equal(t, NodeTypeAgent, orch.Children[len(orch.Children)-1].Type)
	require.Equal(t, "session:kid", orch.Children[len(orch.Children)-1].ID)
}

func TestChildAutopilotsBeatsContradictoryParentAgentID(t *testing.T) {
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/repo", Name: "repo", Path: "/repo", Status: projectstore.StatusOpen,
			Agents: []string{"a", "b"},
		}},
		Autopilots: []*autopilotstore.Autopilot{{
			ID: "ap-1", ProjectID: "/repo", Name: "run", ParentAgentID: "a",
			ManagerAgentID: "mgr",
			Diagnostics:    autopilotstore.Diagnostics{State: "active", Repo: "/repo"},
		}},
		Sessions: []*store.Session{
			{ID: "a", Name: "alpha", ProjectID: "/repo", Repo: "/repo", Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now},
			{ID: "b", Name: "bravo", ProjectID: "/repo", Repo: "/repo", Status: store.StatusIdle, Kind: store.KindAgent, CreatedAt: now.Add(time.Minute), ChildAutopilots: []string{"ap-1"}},
			{ID: "mgr", Role: "autopilot", ProjectID: "/repo", Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: now},
		},
	}
	tree := NewService().Build(in, "")
	require.Len(t, tree.Roots, 1)
	require.Empty(t, projectEntities(tree.Roots[0], NodeTypeAutopilotRun), "owned run must not sit at project root")
	agents := projectEntities(tree.Roots[0], NodeTypeAgent)
	var bravo, alpha *Node
	for _, ch := range agents {
		switch ch.SessionID {
		case "b":
			bravo = ch
		case "a":
			alpha = ch
		}
	}
	require.NotNil(t, bravo)
	require.NotNil(t, alpha)
	require.Len(t, bravo.Children, 1)
	require.Equal(t, "run:ap-1", bravo.Children[0].ID)
	require.Empty(t, alpha.Children)
}

func TestPlanNodeProjectionFields(t *testing.T) {
	now := time.Date(2026, 9, 30, 15, 0, 0, 0, time.UTC)
	in := Inputs{
		Projects: []projectstore.Project{{
			ID: "/p", Name: "p", Path: "/p", Status: projectstore.StatusOpen,
			Plans: []string{"plan-1"},
		}},
		Plans: []*planstore.Plan{{
			ID: "plan-1", ProjectID: "/p", Name: "feat", Status: planstore.PlanStatusInProgress,
			Revision: 4, ExecutionMode: planstore.PlanModeAutopilot, AutopilotRunID: "ap-1",
			Tasks:        []planstore.PlanTask{{ID: "t1", Prompt: "a"}, {ID: "t2", Prompt: "b"}},
			TaskProgress: map[string]string{"t1": "done", "t2": "pending"},
			RepoExport:   &planstore.RepoExportMeta{Revision: 3, ContentHash: "sha256:old"},
			ContentHash:  "sha256:new", UpdatedAt: now,
		}},
	}
	tr := NewService().Build(in, "")
	plans := sectionOf(t, tr.Roots[0], SectionPlans)
	require.Len(t, plans.Children, 1)
	n := plans.Children[0]
	require.NotNil(t, n.Detail)
	require.Equal(t, int64(4), n.Detail.Revision)
	require.Equal(t, "ap-1", n.Detail.ExecutorID)
	require.Equal(t, "1/2", n.Detail.TaskSummary)
	require.Equal(t, "stale", n.Detail.ExportStatus)
	require.Equal(t, now.UTC().Format(time.RFC3339), n.Detail.UpdatedAt)
	require.Empty(t, n.Children, "Plan nodes must not nest executors")
}
