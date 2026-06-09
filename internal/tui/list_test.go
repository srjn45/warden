package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestListWindow(t *testing.T) {
	require.Equal(t, 0, listWindow(3, 0, 10), "n<=visible → 0")
	require.Equal(t, 0, listWindow(10, 2, 5), "cursor within first window → 0")
	require.Equal(t, 1, listWindow(10, 5, 5), "cursor at 5, visible 5 → top 1")
	require.Equal(t, 5, listWindow(10, 9, 5), "cursor at end → n-visible")
	require.Equal(t, 5, listWindow(10, 100, 5), "cursor past end clamps")
	require.Equal(t, 0, listWindow(10, 3, 0), "visible<1 → 0")
}

func TestRenderListContainsAgeColumn(t *testing.T) {
	sessions := []*store.Session{
		{
			ID:        "agent-abc",
			Status:    store.StatusWorking,
			UpdatedAt: time.Now().Add(-30 * time.Second), // 30s ago → "<1m"
			Subject:   "test subject",
		},
	}
	out := renderList(buildItems(sessions, nil), 0, 120, 10)
	require.Contains(t, out, "<1m", "renderList output should contain the age token <1m")
	// Ensure the subject is still present too.
	require.True(t, strings.Contains(out, "test subject") || strings.Contains(out, "test subjec"),
		"renderList output should contain (possibly truncated) subject")
}

func TestRenderListClampsToHeightAndKeepsCursor(t *testing.T) {
	var sessions []*store.Session
	for i := 0; i < 20; i++ {
		sessions = append(sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	out := renderList(buildItems(sessions, nil), 18, 80, 8)
	require.Len(t, strings.Split(out, "\n"), 8, "rendered to exactly height lines")
	require.Contains(t, out, "agent-18", "the selected row is within the window")
	require.Contains(t, out, "more", "a ▲/▼ hint appears when rows are hidden")
}

func TestRenderListShortListPadsToHeight(t *testing.T) {
	sessions := []*store.Session{{ID: "only", Status: store.StatusWorking}}
	require.Len(t, strings.Split(renderList(buildItems(sessions, nil), 0, 80, 6), "\n"), 6, "short list padded to height")
}

func TestRenderListHeightOneRendersSingleLine(t *testing.T) {
	var sessions []*store.Session
	for i := 0; i < 5; i++ {
		sessions = append(sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	require.Len(t, strings.Split(renderList(buildItems(sessions, nil), 0, 80, 1), "\n"), 1, "height 1 with many rows still renders exactly 1 line")
}

func TestSourceDir(t *testing.T) {
	require.Equal(t, "/repo/root", sourceDir(&store.Session{Repo: "/repo/root", Workdir: "/repo/root/.worktrees/x"}), "Repo wins when set")
	require.Equal(t, "/work/proj", sourceDir(&store.Session{Workdir: "/work/proj"}), "falls back to Workdir")
	require.Equal(t, "—", sourceDir(&store.Session{}), "dash when both empty")
}

func TestAbbrevHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, "~", abbrevHome(home), "exact home → ~")
	require.Equal(t, "~/work/proj", abbrevHome(filepath.Join(home, "work", "proj")), "home prefix → ~")
	require.Equal(t, "/etc/thing", abbrevHome("/etc/thing"), "non-home path unchanged")
}

func TestAbbrevHomeWith(t *testing.T) {
	require.Equal(t, "/work/proj", abbrevHomeWith("/work/proj", ""), "empty home (lookup failed) → unchanged")
	require.Equal(t, "~", abbrevHomeWith("/home/me", "/home/me"), "exact home → ~")
	require.Equal(t, "~/x", abbrevHomeWith("/home/me/x", "/home/me"), "home prefix → ~/x")
	require.Equal(t, "/home/meother", abbrevHomeWith("/home/meother", "/home/me"), "no false prefix match")
}

func TestGroupSortOrdersGroupsByRecencyAndKeepsWithinOrder(t *testing.T) {
	now := time.Now()
	in := []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
		{ID: "a2", Workdir: "/a", UpdatedAt: now.Add(-4 * time.Minute)},
	}
	out := groupSort(in)
	got := []string{out[0].ID, out[1].ID, out[2].ID, out[3].ID}
	require.Equal(t, []string{"b1", "b2", "a1", "a2"}, got)
}

func TestGroupSortStableForSingleOrEmpty(t *testing.T) {
	require.Empty(t, groupSort(nil))
	one := []*store.Session{{ID: "x", Workdir: "/a"}}
	require.Equal(t, one, groupSort(one))
}

func TestRenderListGroupsBySourceDir(t *testing.T) {
	sessions := groupSort([]*store.Session{
		{ID: "a1", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: time.Now()},
		{ID: "a2", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: time.Now().Add(-time.Minute)},
		{ID: "b1", Workdir: "/work/beta", Status: store.StatusWorking, UpdatedAt: time.Now().Add(-2 * time.Minute)},
	})
	out := renderList(buildItems(sessions, nil), 0, 120, 12)
	require.Contains(t, out, "/work/alpha (2)", "alpha group header with count")
	require.Contains(t, out, "/work/beta (1)", "beta group header with count")
	require.Contains(t, out, "a1")
	require.Contains(t, out, "b1")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "/work/alpha (2)") {
			require.NotContains(t, line, "›", "group header is never the cursor row")
		}
	}
}

func TestBuildItemsGroupsAgentsNoOpenedDirs(t *testing.T) {
	now := time.Now()
	ss := groupSort([]*store.Session{
		{ID: "a1", Workdir: "/a", UpdatedAt: now},
		{ID: "a2", Workdir: "/a", UpdatedAt: now.Add(-time.Minute)},
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-2 * time.Minute)},
	})
	items := buildItems(ss, nil)
	require.Len(t, items, 3)
	require.Equal(t, "a1", items[0].session.ID)
	require.Equal(t, "a2", items[1].session.ID)
	require.Equal(t, "b1", items[2].session.ID)
	require.Equal(t, "/a", items[0].dir)
}

func TestBuildItemsEmptyOpenedDirGetsPlaceholderOnTop(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-time.Hour)}}
	opened := map[string]time.Time{"/freshly/opened": now} // newer than the agent
	items := buildItems(ss, opened)
	require.Len(t, items, 2, "one placeholder + one agent")
	require.Nil(t, items[0].session, "placeholder is first (most recent)")
	require.Equal(t, "/freshly/opened", items[0].dir)
	require.Equal(t, "a1", items[1].session.ID)
}

func TestBuildItemsOpenedDirWithAgentsHasNoPlaceholder(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{{ID: "a1", Workdir: "/a", UpdatedAt: now}}
	opened := map[string]time.Time{"/a": now.Add(-time.Hour)} // /a already has an agent
	items := buildItems(ss, opened)
	require.Len(t, items, 1, "no placeholder for an opened dir that has agents")
	require.Equal(t, "a1", items[0].session.ID)
}

func TestBuildItemsOnlyOpenedDirsNoSessions(t *testing.T) {
	now := time.Now()
	items := buildItems(nil, map[string]time.Time{"/x": now})
	require.Len(t, items, 1)
	require.Nil(t, items[0].session)
	require.Equal(t, "/x", items[0].dir)
}

func TestItemKeyDistinguishesAgentsFromPlaceholders(t *testing.T) {
	require.Equal(t, "agent-x", itemKey(item{session: &store.Session{ID: "agent-x"}, dir: "/a"}))
	require.Equal(t, dirKey("/a"), itemKey(item{dir: "/a"}))
	require.NotEqual(t, itemKey(item{dir: "/a"}), itemKey(item{session: &store.Session{ID: "/a"}, dir: "/a"}))
}

func TestItemAtClampsOutOfRange(t *testing.T) {
	items := []item{{dir: "/a"}, {session: &store.Session{ID: "x"}, dir: "/a"}}
	require.Equal(t, "/a", itemAt(items, -1).dir, "negative clamps to first")
	require.Equal(t, "x", itemAt(items, 99).session.ID, "past end clamps to last")
	require.Equal(t, item{}, itemAt(nil, 0), "empty list → zero item")
}

func TestActiveDirUsesCursorItemElseFallback(t *testing.T) {
	items := []item{{session: &store.Session{ID: "x"}, dir: "/work/api"}, {dir: "—"}}
	require.Equal(t, "/work/api", activeDir(items, 0, "/fallback"))
	require.Equal(t, "/fallback", activeDir(items, 1, "/fallback"), "unknown dir (—) → fallback")
	require.Equal(t, "/fallback", activeDir(nil, 0, "/fallback"), "no items → fallback")
}

func TestExpandPath(t *testing.T) {
	require.Equal(t, "/home/me", expandPath("~", "/home/me"))
	require.Equal(t, "/home/me/work", expandPath("~/work", "/home/me"))
	require.Equal(t, "/home/me/work", expandPath("~/work/", "/home/me"), "clean strips trailing slash")
	require.Equal(t, "/abs/path", expandPath("/abs/path", "/home/me"), "absolute unchanged")
}

func TestDirCompletionTarget(t *testing.T) {
	d, leaf := dirCompletionTarget("/home/me/wo")
	require.Equal(t, "/home/me", d)
	require.Equal(t, "wo", leaf)

	d, leaf = dirCompletionTarget("/home/me/work/")
	require.Equal(t, "/home/me/work", d)
	require.Equal(t, "", leaf, "trailing slash → list children")

	d, leaf = dirCompletionTarget("/")
	require.Equal(t, "/", d)
	require.Equal(t, "", leaf)
}

func TestLongestCommonPrefix(t *testing.T) {
	require.Equal(t, "ap", longestCommonPrefix([]string{"api", "apex"}))
	require.Equal(t, "only", longestCommonPrefix([]string{"only"}))
	require.Equal(t, "", longestCommonPrefix([]string{"api", "web"}))
	require.Equal(t, "", longestCommonPrefix(nil))
}

func TestCompleteDir(t *testing.T) {
	listing := client.DirListing{
		Path: "/home/me/work",
		Entries: []client.DirEntry{
			{Name: "api", Path: "/home/me/work/api"},
			{Name: "apex", Path: "/home/me/work/apex"},
			{Name: "web", Path: "/home/me/work/web"},
		},
	}
	completed, cands := completeDir(listing, "/home/me/work/ap")
	require.Equal(t, "/home/me/work/ap", completed, "completes to longest common prefix of api+apex")
	require.Equal(t, []string{"api", "apex"}, cands)

	completed, cands = completeDir(listing, "/home/me/work/web")
	require.Equal(t, "/home/me/work/web", completed, "single match completes fully")
	require.Equal(t, []string{"web"}, cands)

	completed, cands = completeDir(listing, "/home/me/work/zzz")
	require.Equal(t, "/home/me/work/zzz", completed, "no match → unchanged")
	require.Nil(t, cands)
}

func TestRenderListShowsPlaceholderForEmptyOpenedDir(t *testing.T) {
	now := time.Now()
	items := buildItems(
		[]*store.Session{{ID: "a1", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: now.Add(-time.Hour)}},
		map[string]time.Time{"/work/empty": now},
	)
	out := renderList(items, 0, 120, 12)
	require.Contains(t, out, "/work/empty (0)", "empty opened dir header with zero count")
	require.Contains(t, out, "no agents", "placeholder hint line")
}

func TestCompleteDirAdvancesAndPreservesTyped(t *testing.T) {
	listing := client.DirListing{
		Path: "/home/me/work",
		Entries: []client.DirEntry{
			{Name: "api", Path: "/home/me/work/api"},
			{Name: "apex", Path: "/home/me/work/apex"},
			{Name: "web", Path: "/home/me/work/web"},
		},
	}
	// advances the cursor from a shorter leaf to the longest common prefix
	completed, cands := completeDir(listing, "/home/me/work/a")
	require.Equal(t, "/home/me/work/ap", completed, "advances 'a' → 'ap'")
	require.Equal(t, []string{"api", "apex"}, cands)

	// trailing slash, no common prefix among children → typed preserved (slash kept)
	completed, cands = completeDir(listing, "/home/me/work/")
	require.Equal(t, "/home/me/work/", completed, "nothing to advance → typed unchanged, slash preserved")
	require.Equal(t, []string{"api", "apex", "web"}, cands)
}

func TestBuildRowsIncludesApprovalsRow(t *testing.T) {
	items := []item{
		{approvals: true, apprCount: 2},
		{session: &store.Session{ID: "a1"}, dir: "/repo"},
	}
	rows := buildRows(items)
	require.Equal(t, "", rows[0].header)
	require.Equal(t, 0, rows[0].idx)
	require.NotEqual(t, "", rows[1].header)
}

func TestItemKeyApprovals(t *testing.T) {
	require.Equal(t, "approvals\x00", itemKey(item{approvals: true}))
}

func TestRenderListGroupedSmallHeightKeepsCursor(t *testing.T) {
	now := time.Now()
	var ss []*store.Session
	for i := 0; i < 6; i++ {
		ss = append(ss, &store.Session{
			ID:        fmt.Sprintf("a%d", i),
			Workdir:   "/work/alpha",
			Status:    store.StatusWorking,
			UpdatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}
	sessions := groupSort(ss)
	for h := 1; h <= 4; h++ {
		out := renderList(buildItems(sessions, nil), 5, 80, h)
		require.Len(t, strings.Split(out, "\n"), h, "exactly height lines at h=%d", h)
		require.Contains(t, out, "a5", "selected agent must stay visible at h=%d", h)
	}
}

func TestPipelineItems(t *testing.T) {
	ps := []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}, {ID: "b", Status: pipeline.JobPending}}}}
	items := pipelineItems(ps, nil)
	if len(items) != 3 {
		t.Fatalf("want 3 items (1 pipeline + 2 jobs), got %d", len(items))
	}
	if items[0].pipeline == nil || items[0].pipeline.ID != "demo" {
		t.Fatalf("first item should be the pipeline header: %+v", items[0])
	}
	if items[1].pjJob == nil || items[1].pjJob.ID != "a" || items[1].pjPipe != "demo" {
		t.Fatalf("second item should be job a: %+v", items[1])
	}
	// distinct job pointers (not aliasing the same loop var).
	if items[1].pjJob == items[2].pjJob {
		t.Fatalf("job items must hold distinct pointers")
	}
}

func TestItemsPrependsPipelinesAndFiltersOwnedSessions(t *testing.T) {
	m := newListPane(&fakeAPI{}, "")
	m.sessions = []*store.Session{
		{ID: "free", Status: store.StatusWorking},
		{ID: "demo-a", Status: store.StatusWorking, PipelineID: "demo", JobID: "a"},
	}
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning, SessionID: "demo-a"}}}}
	items := m.items()
	// pipeline header + its 1 job + the free session; the pipeline-owned session is filtered out.
	var sawPipe, sawJob, sawFree, sawOwned bool
	for _, it := range items {
		if it.pipeline != nil {
			sawPipe = true
		}
		if it.pjJob != nil {
			sawJob = true
		}
		if it.session != nil && it.session.ID == "free" {
			sawFree = true
		}
		if it.session != nil && it.session.ID == "demo-a" {
			sawOwned = true
		}
	}
	if !sawPipe || !sawJob || !sawFree {
		t.Fatalf("missing rows: pipe=%v job=%v free=%v", sawPipe, sawJob, sawFree)
	}
	if sawOwned {
		t.Fatalf("pipeline-owned session must not appear as a flat session row")
	}
}

func TestRenderItemLinePipelineRows(t *testing.T) {
	// expanded (default): ▾ indicator
	head := renderItemLine(item{pipeline: &pipeline.Pipeline{ID: "demo", Status: pipeline.StatusRunning}}, false, 60)
	if !strings.Contains(head, "demo") || !strings.Contains(head, "▾") || !strings.Contains(head, "running") {
		t.Fatalf("expanded pipeline header row wrong: %q", head)
	}
	// collapsed: ▸ indicator
	col := renderItemLine(item{pipeline: &pipeline.Pipeline{ID: "demo", Status: pipeline.StatusRunning}, collapsed: true}, false, 60)
	if !strings.Contains(col, "▸") {
		t.Fatalf("collapsed pipeline header row should show ▸: %q", col)
	}
	glyph, _ := jobBadge(pipeline.JobDone)
	jobRow := renderItemLine(item{pjPipe: "demo", pjJob: &pipeline.Job{ID: "a", Status: pipeline.JobDone, DependsOn: []string{"x"}}}, false, 60)
	if !strings.Contains(jobRow, "a") || !strings.Contains(jobRow, glyph) || !strings.Contains(jobRow, "x") {
		t.Fatalf("job row wrong: %q", jobRow)
	}
}

func TestJobBadge(t *testing.T) {
	cases := []struct {
		status pipeline.JobStatus
		glyph  string
		color  lipgloss.Color
	}{
		{pipeline.JobPending, "○", lipgloss.Color("8")},        // grey
		{pipeline.JobRunning, "◐", lipgloss.Color("6")},        // cyan
		{pipeline.JobDone, "●", lipgloss.Color("2")},           // green
		{pipeline.JobFailed, "✗", lipgloss.Color("1")},         // red
		{pipeline.JobNeedsAttention, "⚠", lipgloss.Color("3")}, // amber
		{pipeline.JobSkipped, "⊘", lipgloss.Color("8")},        // grey
	}
	seen := map[string]bool{}
	for _, c := range cases {
		glyph, st := jobBadge(c.status)
		if glyph != c.glyph {
			t.Errorf("jobBadge(%s) glyph = %q, want %q", c.status, glyph, c.glyph)
		}
		if got := st.GetForeground(); got != c.color {
			t.Errorf("jobBadge(%s) color = %v, want %v", c.status, got, c.color)
		}
		if seen[glyph] {
			t.Errorf("jobBadge(%s) glyph %q is a duplicate", c.status, glyph)
		}
		seen[glyph] = true
	}
}

func pipeModel() listPaneModel {
	m := newListPane(&fakeAPI{}, "")
	m.ready = true
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "a", Status: pipeline.JobFailed, SessionID: "demo-a"},
		{ID: "b", Status: pipeline.JobRunning, SessionID: "demo-b"},
	}}}
	return m
}

func TestKeyCancelPipeline(t *testing.T) {
	m := pipeModel()
	m.cursor = 0 // the pipeline header row
	updated, cmd := m.handleKey(key("x"))
	if cmd == nil {
		t.Fatalf("x on a pipeline row should return a cancel cmd")
	}
	cmd() // runs the command (calls the fake api)
	fa := updated.(listPaneModel).api.(*fakeAPI)
	if fa.canceled != "demo" {
		t.Fatalf("want canceled=demo, got %q", fa.canceled)
	}
}

func TestKeyRetryFailedJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 1 // job "a" (failed)
	_, cmd := m.handleKey(key("r"))
	if cmd == nil {
		t.Fatalf("r on a failed job should return a retry cmd")
	}
	cmd()
	fa := m.api.(*fakeAPI)
	if fa.retried != "demo/a" {
		t.Fatalf("want retried=demo/a, got %q", fa.retried)
	}
}

func TestKeyRetryIgnoredOnRunningJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 2 // job "b" (running) — not retryable
	_, cmd := m.handleKey(key("r"))
	if cmd != nil {
		t.Fatalf("r on a running job should be a no-op")
	}
}

func TestKeyAttachRunningJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 2 // job "b" (running, has a session)
	_, cmd := m.handleKey(key("a"))
	if cmd == nil {
		t.Fatalf("a on a running job should return an attach cmd")
	}
}

func TestBuildRowsNoHeaderForPipelineRows(t *testing.T) {
	items := []item{
		{pipeline: &pipeline.Pipeline{ID: "demo", Status: pipeline.StatusRunning}},
		{pjPipe: "demo", pjJob: &pipeline.Job{ID: "a", Status: pipeline.JobRunning}},
		{session: &store.Session{ID: "free"}, dir: "/work"},
	}
	for _, r := range buildRows(items) {
		if strings.Contains(r.header, "(0)") {
			t.Fatalf("spurious empty group header above pipeline rows: %q", r.header)
		}
	}
}

func TestPipelineDisplayStatus(t *testing.T) {
	job := func(s pipeline.JobStatus) pipeline.Job { return pipeline.Job{ID: "j", Status: s} }
	cases := []struct {
		name  string
		p     *pipeline.Pipeline
		label string
		glyph string
		color lipgloss.Color
	}{
		{"pending", &pipeline.Pipeline{Status: pipeline.StatusPending}, "pending", "○", lipgloss.Color("8")},
		{"running", &pipeline.Pipeline{Status: pipeline.StatusRunning}, "running", "◐", lipgloss.Color("6")},
		{"done", &pipeline.Pipeline{Status: pipeline.StatusDone, Jobs: []pipeline.Job{job(pipeline.JobDone)}}, "done", "●", lipgloss.Color("2")},
		{"stalled", &pipeline.Pipeline{Status: pipeline.StatusStalled, Jobs: []pipeline.Job{job(pipeline.JobDone), job(pipeline.JobSkipped)}}, "stalled", "⚠", lipgloss.Color("3")},
		{"canceled", &pipeline.Pipeline{Status: pipeline.StatusCanceled, Jobs: []pipeline.Job{job(pipeline.JobDone)}}, "canceled", "⊘", lipgloss.Color("8")},
		// derived partial: terminal pipeline with a failed / needs_attention job.
		{"partial-done-failed", &pipeline.Pipeline{Status: pipeline.StatusDone, Jobs: []pipeline.Job{job(pipeline.JobDone), job(pipeline.JobFailed)}}, "partial", "◑", lipgloss.Color("3")},
		{"partial-stalled-needs", &pipeline.Pipeline{Status: pipeline.StatusStalled, Jobs: []pipeline.Job{job(pipeline.JobDone), job(pipeline.JobNeedsAttention)}}, "partial", "◑", lipgloss.Color("3")},
		{"partial-canceled-failed", &pipeline.Pipeline{Status: pipeline.StatusCanceled, Jobs: []pipeline.Job{job(pipeline.JobFailed)}}, "partial", "◑", lipgloss.Color("3")},
		// negative: a failed job on a NON-terminal pipeline is not "partial".
		{"running-with-failed-not-partial", &pipeline.Pipeline{Status: pipeline.StatusRunning, Jobs: []pipeline.Job{job(pipeline.JobFailed)}}, "running", "◐", lipgloss.Color("6")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, st, glyph := pipelineDisplayStatus(c.p)
			require.Equal(t, c.label, label)
			require.Equal(t, c.glyph, glyph)
			require.Equal(t, c.color, st.GetForeground())
		})
	}
}

func TestPipelineItemsHonorsCollapsed(t *testing.T) {
	ps := []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}, {ID: "b", Status: pipeline.JobPending}}}}

	// collapsed → header only.
	items := pipelineItems(ps, map[string]bool{"demo": true})
	require.Len(t, items, 1, "collapsed pipeline emits only its header row")
	require.NotNil(t, items[0].pipeline)
	require.True(t, items[0].collapsed, "header item carries collapsed state")

	// expanded (explicit false) → header + jobs.
	items = pipelineItems(ps, map[string]bool{"demo": false})
	require.Len(t, items, 3, "expanded pipeline emits header + job rows")
	require.False(t, items[0].collapsed)
}

func TestKeyCollapseExpandPipeline(t *testing.T) {
	m := pipeModel()
	m.cursor = 0 // pipeline header

	// collapse with h
	updated, _ := m.handleKey(key("h"))
	mc := updated.(listPaneModel)
	require.True(t, mc.collapsed["demo"], "h collapses the pipeline under the cursor")
	require.Len(t, mc.items(), 1, "collapsed → only the header remains")

	// expand with l
	updated, _ = mc.handleKey(key("l"))
	me := updated.(listPaneModel)
	require.False(t, me.collapsed["demo"], "l expands the pipeline under the cursor")
	require.Len(t, me.items(), 3, "expanded → header + 2 jobs")
}

func TestKeyCollapseFromJobRepinsCursorToHeader(t *testing.T) {
	m := pipeModel()
	m.cursor = 1 // job "a" (a hidden row once collapsed)

	updated, _ := m.handleKey(key("h"))
	mc := updated.(listPaneModel)
	require.True(t, mc.collapsed["demo"], "h on a job collapses its parent pipeline")
	require.Equal(t, 0, mc.cursor, "cursor re-pinned to the header, never a hidden row")
	require.NotNil(t, itemAt(mc.items(), mc.cursor).pipeline, "cursor lands on the pipeline header")
}

func TestFlatSessionsIncludesOrphanedPipelineAgents(t *testing.T) {
	sessions := []*store.Session{
		{ID: "plain"}, // not pipeline-owned → flat
		{ID: "p1-a", PipelineID: "p1", JobID: "a"},     // owned by a live pipeline → not flat
		{ID: "gone-x", PipelineID: "gone", JobID: "x"}, // pipeline deleted → orphan, must be flat
	}
	pipelines := []*pipeline.Pipeline{{ID: "p1"}}

	got := flatSessions(sessions, pipelines)

	ids := make([]string, len(got))
	for i, s := range got {
		ids[i] = s.ID
	}
	require.ElementsMatch(t, []string{"plain", "gone-x"}, ids,
		"flat list must include un-owned agents and orphans of deleted pipelines, but not live-pipeline-owned agents")
}
