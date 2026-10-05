package tree

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// Zero-touch auto-registration (daemon ensureOpenProject): a launch in a repo
// creates a project with ID == Path == the repo root and appends the member to
// the matching Project list. These tests pin that such a project renders as an
// open project node (catOpenProject), never as a loose dir.

func autoProject(root string, mut func(*projectstore.Project)) projectstore.Project {
	p := projectstore.Project{ID: root, Name: "warden", Path: root, Status: projectstore.StatusOpen}
	if mut != nil {
		mut(&p)
	}
	return p
}

func TestAutoRegistered_FreshProjectWithOneAgent(t *testing.T) {
	root := "/home/u/dev/warden"
	in := Inputs{
		Projects: []projectstore.Project{autoProject(root, func(p *projectstore.Project) { p.Agents = []string{"a1"} })},
		Sessions: []*store.Session{
			{ID: "a1", Name: "worker", ProjectID: root, Repo: root, Status: store.StatusWorking, Kind: store.KindAgent, CreatedAt: time.Now()},
		},
	}
	tr := NewService().Build(in, "")

	require.Len(t, tr.Roots, 1, "no loose-dir root alongside the project")
	proj := tr.Roots[0]
	require.Equal(t, "project:"+root, proj.ID)
	require.False(t, proj.Detail.Closed)
	require.False(t, proj.Detail.Synthetic)
	projectByID := map[string]projectstore.Project{root: in.Projects[0]}
	require.Equal(t, catOpenProject, categoryOf(proj, projectByID))
	agents := projectEntities(proj, NodeTypeAgent)
	require.Len(t, agents, 1)
	require.Equal(t, "session:a1", agents[0].ID)
}

func TestAutoRegistered_WorktreeAgentUnderParentProject(t *testing.T) {
	root := "/home/u/dev/warden"
	wt := root + "/.worktrees/feature-x"
	cases := map[string]*store.Session{
		"repo-and-workdir-in-worktree": {ID: "w1", ProjectID: root, Repo: wt, Workdir: wt},
		"workdir-only-in-worktree":     {ID: "w1", ProjectID: root, Workdir: wt},
		"legacy-path-only (no id)":     {ID: "w1", Repo: wt},
	}
	for name, sess := range cases {
		t.Run(name, func(t *testing.T) {
			sess.Kind, sess.Status, sess.CreatedAt = store.KindAgent, store.StatusWorking, time.Now()
			proj := autoProject(root, nil)
			if sess.ProjectID != "" {
				proj.Agents = []string{"w1"}
			}
			tr := NewService().Build(Inputs{Projects: []projectstore.Project{proj}, Sessions: []*store.Session{sess}}, "")

			require.Len(t, tr.Roots, 1, "worktree checkout must not create a loose-dir root")
			require.Equal(t, "project:"+root, tr.Roots[0].ID)
			require.Equal(t, catOpenProject, categoryOf(tr.Roots[0], map[string]projectstore.Project{root: proj}))
			agents := projectEntities(tr.Roots[0], NodeTypeAgent)
			require.Len(t, agents, 1)
			require.Equal(t, "session:w1", agents[0].ID)
		})
	}
}

func TestAutoRegistered_PipelineAndTerminalBoundToProject(t *testing.T) {
	root := "/home/u/dev/warden"
	proj := autoProject(root, func(p *projectstore.Project) {
		p.Pipelines = []string{"pipe1"}
		p.Terminals = []string{"term1"}
	})
	in := Inputs{
		Projects: []projectstore.Project{proj},
		Pipelines: []*pipeline.Pipeline{
			{ID: "pipe1", Name: "pipe1", Repo: root, ProjectID: root, Status: pipeline.StatusRunning,
				Jobs: []pipeline.Job{{ID: "j", Status: pipeline.JobPending, DependsOn: []string{}}}},
		},
		Sessions: []*store.Session{
			{ID: "term1", Name: "sh", ProjectID: root, Workdir: root + "/.worktrees/x", Status: store.StatusIdle, Kind: store.KindTerminal, CreatedAt: time.Now()},
		},
	}
	tr := NewService().Build(in, "")

	require.Len(t, tr.Roots, 1)
	p := tr.Roots[0]
	require.Equal(t, "project:"+root, p.ID)
	require.Equal(t, catOpenProject, categoryOf(p, map[string]projectstore.Project{root: proj}))
	pipes := projectEntities(p, NodeTypePipeline)
	require.Len(t, pipes, 1)
	require.Equal(t, "pipeline:pipe1", pipes[0].ID)
	terms := sectionOf(t, p, SectionTerminals)
	require.Len(t, terms.Children, 1)
	require.Equal(t, "session:term1", terms.Children[0].ID)
}

// categoryOf mirrors sortRoots' categorisation for a built root node.
func categoryOf(n *Node, projectByID map[string]projectstore.Project) rootCategory {
	if n.Detail != nil && n.Detail.Synthetic {
		return catSynthetic
	}
	raw := n.ID[len("project:"):]
	if _, ok := projectByID[raw]; ok {
		if n.Detail != nil && n.Detail.Closed {
			return catClosedProject
		}
		return catOpenProject
	}
	return catLooseDir
}
