package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// findPipeline / findJobIDs are small readers over the woven item stream.
func findPipeline(items []item, id string) *item {
	for i := range items {
		if items[i].pipeline != nil && items[i].pipeline.ID == id {
			return &items[i]
		}
	}
	return nil
}

func findAgent(items []item, id string) *item {
	for i := range items {
		if items[i].session != nil && items[i].session.ID == id {
			return &items[i]
		}
	}
	return nil
}

func jobIDsOf(items []item) []string {
	var ids []string
	for i := range items {
		if items[i].pjJob != nil {
			ids = append(ids, items[i].pjJob.ID)
		}
	}
	return ids
}

func dirHeaderCount(items []item) int {
	n := 0
	for _, r := range buildRows(items) {
		if r.header != "" {
			n++
		}
	}
	return n
}

// C3 acceptance: a delegated pipeline (A3 owner link) nests under its owning
// orchestrator — a sibling of that agent's subagents (one level deeper), sharing
// the owner's project node — and its job children render beneath it in order.
func TestBuildProjectTreeDelegatedPipelineNestsUnderOwner(t *testing.T) {
	now := time.Now()
	orch := &store.Session{ID: "orch", Repo: "/repoX", Status: store.StatusWorking, CreatedAt: now}
	p := &pipeline.Pipeline{ID: "deleg", OwnerID: "orch", Repo: "/repoX", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "j1", Status: pipeline.JobRunning},
			{ID: "j2", Status: pipeline.JobPending, DependsOn: []string{"j1"}},
		}}
	items := buildProjectTree([]*store.Session{orch}, []*pipeline.Pipeline{p}, nil, nil, keyByDir, nil)

	orchIt := findAgent(items, "orch")
	require.NotNil(t, orchIt)
	require.True(t, orchIt.hasKids, "owning an agent-less delegated pipeline makes the orchestrator collapsible")

	pipeIt := findPipeline(items, "deleg")
	require.NotNil(t, pipeIt, "the delegated pipeline renders inside the Projects frame")
	require.Equal(t, 1, pipeIt.depth, "delegated pipeline nests one level under its owner")
	require.Equal(t, orchIt.dir, pipeIt.dir, "delegated pipeline shares the owner's project node")
	require.Equal(t, []string{"j1", "j2"}, jobIDsOf(items), "job children render under the pipeline, in order")
}

// C3 acceptance: a human/orchestrator-less pipeline (no owner) renders directly
// under its project node, a sibling of that project's top-level agents, after them.
func TestBuildProjectTreeHumanPipelineUnderProjectNode(t *testing.T) {
	now := time.Now()
	a := &store.Session{ID: "a1", Repo: "/repoX", Status: store.StatusWorking, CreatedAt: now}
	p := &pipeline.Pipeline{ID: "human", Repo: "/repoX", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "only", Status: pipeline.JobRunning}}}
	items := buildProjectTree([]*store.Session{a}, []*pipeline.Pipeline{p}, nil, nil, keyByDir, nil)

	require.False(t, findAgent(items, "a1").hasKids, "an unrelated agent does not adopt the human pipeline")
	pipeIt := findPipeline(items, "human")
	require.NotNil(t, pipeIt)
	require.Equal(t, 0, pipeIt.depth, "a human pipeline homes at the project node, level with the agents")
	require.Equal(t, "/repoX", pipeIt.dir, "under its project (keyed by repo)")

	// The agent and the pipeline share one project header (the pipeline isn't a
	// separate project), and the pipeline follows the agent.
	require.Equal(t, 1, dirHeaderCount(items), "agent + human pipeline share one project header")
	aIdx, pIdx := -1, -1
	for i := range items {
		if items[i].session != nil && items[i].session.ID == "a1" {
			aIdx = i
		}
		if items[i].pipeline != nil {
			pIdx = i
		}
	}
	require.Greater(t, pIdx, aIdx, "the human pipeline renders after the project's agents")
}

// C3: a pipeline whose project has no agents still gets a project node of its own —
// its pipeline renders under a project header, not as an empty placeholder.
func TestBuildProjectTreeHumanPipelineOnlyProject(t *testing.T) {
	p := &pipeline.Pipeline{ID: "solo", Repo: "/repoY", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "j", Status: pipeline.JobRunning}}}
	items := buildProjectTree(nil, []*pipeline.Pipeline{p}, nil, nil, keyByDir, nil)

	pipeIt := findPipeline(items, "solo")
	require.NotNil(t, pipeIt, "the pipeline renders even with no agents in its project")
	require.Equal(t, "/repoY", pipeIt.dir)
	for _, it := range items {
		if it.session == nil && it.pipeline == nil && it.pjJob == nil {
			t.Fatalf("a pipeline-only project must not emit an empty placeholder row")
		}
	}
	require.Equal(t, 1, dirHeaderCount(items), "the pipeline-only project still gets a project node header")
}

// C3: anchored projects (with agents / opened dirs) sort before pipeline-only ones,
// so real work stays at the top and a bare CLI pipeline lands beneath it.
func TestBuildProjectTreePipelineOnlyProjectSortsAfterAgentProjects(t *testing.T) {
	now := time.Now()
	a := &store.Session{ID: "a1", Repo: "/repoAgents", Status: store.StatusWorking, CreatedAt: now}
	p := &pipeline.Pipeline{ID: "solo", Repo: "/repoPipeOnly", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "j", Status: pipeline.JobRunning}}}
	items := buildProjectTree([]*store.Session{a}, []*pipeline.Pipeline{p}, nil, nil, keyByDir, nil)
	require.Less(t, indexOfAgent(items, "a1"), indexOfPipeline(items, "solo"),
		"the agent project renders before the pipeline-only project")
}

func indexOfAgent(items []item, id string) int {
	for i := range items {
		if items[i].session != nil && items[i].session.ID == id {
			return i
		}
	}
	return -1
}

func indexOfPipeline(items []item, id string) int {
	for i := range items {
		if items[i].pipeline != nil && items[i].pipeline.ID == id {
			return i
		}
	}
	return -1
}

// C3: collapsing the owning orchestrator folds away its delegated pipeline too —
// the pipeline is part of the owner's sub-tree.
func TestBuildProjectTreeDelegatedPipelineHiddenWhenOwnerCollapsed(t *testing.T) {
	orch := &store.Session{ID: "orch", Repo: "/repoX", Status: store.StatusWorking}
	p := &pipeline.Pipeline{ID: "deleg", OwnerID: "orch", Repo: "/repoX", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "j1", Status: pipeline.JobRunning}}}
	items := buildProjectTree([]*store.Session{orch}, []*pipeline.Pipeline{p}, nil, map[string]bool{"orch": true}, keyByDir, nil)

	require.Nil(t, findPipeline(items, "deleg"), "collapsing the owner hides its delegated pipeline")
	require.Empty(t, jobIDsOf(items), "and the pipeline's jobs with it")
	require.True(t, findAgent(items, "orch").collapsed, "the owner still shows as a collapsed node")
}

// C3: a pipeline's own collapse (collapse-completed default, or a manual fold)
// hides its jobs while keeping the pipeline node itself visible.
func TestBuildProjectTreePipelineOwnCollapseHidesJobs(t *testing.T) {
	orch := &store.Session{ID: "orch", Repo: "/repoX", Status: store.StatusWorking}
	p := &pipeline.Pipeline{ID: "deleg", OwnerID: "orch", Repo: "/repoX", Status: pipeline.StatusDone,
		Jobs: []pipeline.Job{{ID: "j1", Status: pipeline.JobDone}}}
	items := buildProjectTree([]*store.Session{orch}, []*pipeline.Pipeline{p}, nil, map[string]bool{"deleg": true}, keyByDir, nil)

	pipeIt := findPipeline(items, "deleg")
	require.NotNil(t, pipeIt, "the collapsed pipeline header still shows")
	require.True(t, pipeIt.collapsed)
	require.Empty(t, jobIDsOf(items), "a collapsed pipeline hides its job rows")
}

// C3: a delegated pipeline whose owner is not in the session set (reaped) falls
// back to homing under its project node, so it never vanishes.
func TestBuildProjectTreeOrphanDelegatedFallsBackToProject(t *testing.T) {
	a := &store.Session{ID: "a1", Repo: "/repoX", Status: store.StatusWorking}
	p := &pipeline.Pipeline{ID: "deleg", OwnerID: "ghost", Repo: "/repoX", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "j", Status: pipeline.JobRunning}}}
	items := buildProjectTree([]*store.Session{a}, []*pipeline.Pipeline{p}, nil, nil, keyByDir, nil)

	require.False(t, findAgent(items, "a1").hasKids, "an unrelated agent is not treated as the owner")
	pipeIt := findPipeline(items, "deleg")
	require.NotNil(t, pipeIt)
	require.Equal(t, 0, pipeIt.depth, "an owner-less-in-tree pipeline homes at the project node")
	require.Equal(t, "/repoX", pipeIt.dir)
}

// C3: the woven job rows keep their (deps: …) annotation and nest visibly under
// their delegated pipeline (indented one level deeper than the pipeline node).
func TestRenderNestedPipelineShowsDepsAndNesting(t *testing.T) {
	orch := &store.Session{ID: "orch", Repo: "/repoX", Status: store.StatusWorking}
	p := &pipeline.Pipeline{ID: "deleg", OwnerID: "orch", Repo: "/repoX", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "build", Status: pipeline.JobRunning},
			{ID: "test", Status: pipeline.JobPending, DependsOn: []string{"build"}},
		}}
	items := buildProjectTree([]*store.Session{orch}, []*pipeline.Pipeline{p}, nil, nil, keyByDir, nil)
	out := renderList(items, 0, 140, 16)
	require.Contains(t, out, "deleg", "the delegated pipeline node renders")
	require.Contains(t, out, "(deps: build)", "the job deps annotation is preserved")

	// The nested pipeline node indents deeper than a depth-0 project pipeline: a
	// delegated pipeline row (depth 1) carries the subagent-level indent.
	pipeLine := renderItemLine(*findPipeline(items, "deleg"), false, 140)
	require.True(t, strings.HasPrefix(strings.TrimPrefix(pipeLine, "  "), nestIndent(1)+"▾"),
		"the delegated pipeline row is indented one sub-tree level under its owner: %q", pipeLine)
}
