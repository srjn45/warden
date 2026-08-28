package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// oneProjectKey maps several worktree dirs of one repo to a single project key,
// standing in for the B2 normalizer's remote-URL collapsing.
func oneProjectKey(dir string) string {
	switch dir {
	case "/repo/wt1", "/repo/wt2":
		return "github.com/org/repo"
	default:
		return dir
	}
}

// C2: two worktrees of one repo collapse to a single project node — the agents in
// each worktree group under one header (the first worktree's dir), not two.
func TestBuildItemsCollapsesWorktreesByProjectKey(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "a1", Repo: "/repo/wt1", Status: store.StatusWorking, CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "a2", Repo: "/repo/wt2", Status: store.StatusWorking, CreatedAt: now.Add(-time.Minute)},
		{ID: "b1", Repo: "/other", Status: store.StatusWorking, CreatedAt: now},
	}
	items := buildItems(ss, nil, nil, oneProjectKey)

	byID := map[string]item{}
	for _, it := range items {
		if it.session != nil {
			byID[it.session.ID] = it
		}
	}
	require.Equal(t, byID["a1"].dir, byID["a2"].dir, "both worktrees share one project node's dir")
	require.NotEqual(t, byID["a1"].dir, byID["b1"].dir, "a different repo stays a separate project node")

	out := renderList(items, 0, 120, 20)
	require.Contains(t, out, "a1")
	require.Contains(t, out, "a2")
	require.Contains(t, out, "b1")
	// The collapsed repo renders as one 2-agent project node, not two 1-agent ones.
	require.Contains(t, out, "(2)", "the two worktrees form a single 2-agent project node")
	require.Equal(t, 1, strings.Count(out, "(2)"), "exactly one collapsed 2-agent node")
}

// C2 / §4.1: a child in a SIBLING worktree of the same project still nests under
// its parent (they share a project key), rather than surfacing as a root.
func TestBuildItemsChildNestsAcrossWorktreesOfSameProject(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "parent", Repo: "/repo/wt1", Status: store.StatusWorking, CreatedAt: now},
		{ID: "child", ParentID: "parent", Repo: "/repo/wt2", Status: store.StatusWorking, CreatedAt: now.Add(time.Minute)},
	}
	items := buildItems(ss, nil, nil, oneProjectKey)
	require.Equal(t, []string{"parent", "child"}, itemSessionIDs(items))
	require.Equal(t, 1, items[1].depth, "child in a sibling worktree of the same project nests one level")
	require.Equal(t, "", items[1].fromParent, "same-project nesting carries no cross-project backlink")
}

// C2: the control pane composes each top-level section as its own bordered/titled
// inner frame — a nested Projects and Terminals layout inside the Control frame.
func TestRenderFramesNestedProjectsAndTerminals(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.sessions = groupSort([]*store.Session{
		{ID: "a1", Repo: "/repoA", Status: store.StatusWorking},
		{ID: "t1", Kind: store.KindTerminal, Workdir: "/w", Status: store.StatusWorking},
	})
	items := m.items()
	out := renderFrames(items, 0, 60, 21)

	require.Contains(t, out, "Projects", "a titled Projects inner frame")
	require.Contains(t, out, "Terminals", "a titled Terminals inner frame")
	require.NotContains(t, out, "Pipelines", "no top-level Pipelines frame (C3 folds it into Projects)")
	require.NotContains(t, out, "Agents", "the Agents frame is renamed Projects")
	// Bordered: rounded box corners frame each section.
	require.Contains(t, out, "╭", "inner frames are bordered")
	require.Contains(t, out, "╰", "inner frames are bordered")
	require.Contains(t, out, "a1", "the agent renders inside the Projects frame")
	require.Len(t, strings.Split(out, "\n"), 21, "the frames fill exactly the requested height")
}

// C2: a collapsed frame shows only its titled border (▸ glyph), hiding its rows;
// the cursor-on-header caret keeps folding discoverable now the header is a title.
func TestRenderFramesCollapsedFrameShowsTitleOnly(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.sessions = groupSort([]*store.Session{{ID: "a1", Repo: "/repoA", Status: store.StatusWorking}})
	m.collapsed[secKey(secProjects)] = true
	items := m.items()
	cur := cursorOn(m, func(it item) bool { return it.section == secProjects })

	out := renderFrames(items, cur, 60, 21)
	require.Contains(t, out, "▸ Projects", "a collapsed frame shows the ▸ collapse glyph in its title")
	require.NotContains(t, out, "a1", "a collapsed Projects frame hides its agents")
	require.Contains(t, out, "› ▸ Projects", "the cursor caret marks the header it sits on")
}

func TestSplitFrameHeights(t *testing.T) {
	sum := func(h []int) int {
		n := 0
		for _, v := range h {
			n += v
		}
		return n
	}
	require.Equal(t, []int{3, 3, 3}, splitFrameHeights([]int{0, 0, 0}, 9), "floors at 3 each with no surplus")

	h := splitFrameHeights([]int{1, 5, 0}, 20)
	require.Equal(t, 20, sum(h), "heights sum to the requested total")
	require.Greater(t, h[1], h[0], "the busier frame gets more room")
	require.GreaterOrEqual(t, h[2], 3, "even an empty frame keeps its floor")

	require.Equal(t, []int{3, 3}, splitFrameHeights([]int{4, 4}, 4), "too little space → floors (outer box clips)")
}
