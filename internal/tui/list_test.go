package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

func TestRenderListRowShowsLeanColumns(t *testing.T) {
	sessions := []*store.Session{
		{
			ID:        "agent-abc",
			Status:    store.StatusWorking,
			UpdatedAt: time.Now().Add(-30 * time.Second), // 30s ago → "<1m"
			Subject:   "this subject must not appear in the row",
		},
	}
	out := renderList(buildItems(sessions, nil, nil), 0, 120, 10)
	require.Contains(t, out, "agent-abc", "row should show the ID")
	require.Contains(t, out, "<1m", "row should show the age token")
	require.NotContains(t, out, "this subject", "subject must not be rendered in the lean row")
}

func TestRenderListRowShowsFullUntrimmedID(t *testing.T) {
	// A full prompt-spawned id is "agent-" + 8 hex = 14 chars; it must appear
	// in full, never truncated with an ellipsis.
	sessions := []*store.Session{
		{ID: "agent-fd56deb4", Status: store.StatusWorking, UpdatedAt: time.Now()},
	}
	out := renderList(buildItems(sessions, nil, nil), 0, 120, 10)
	require.Contains(t, out, "agent-fd56deb4", "the full agent id must render untrimmed")
	require.NotContains(t, out, "…", "the id must not be ellipsis-truncated")
}

func TestRenderListRowShowsBackend(t *testing.T) {
	sessions := []*store.Session{
		{ID: "agent-aider", Status: store.StatusWorking, Backend: "aider", UpdatedAt: time.Now()},
	}
	out := renderList(buildItems(sessions, nil, nil), 0, 120, 10)
	require.Contains(t, out, "aider", "row should show the agent backend")
}

func TestRenderListRowEmptyBackendDefaultsToClaude(t *testing.T) {
	// Backend is omitempty: pre-#52 Claude agents carry no value and must render
	// as "claude" rather than a blank column.
	sessions := []*store.Session{
		{ID: "agent-legacy", Status: store.StatusWorking, UpdatedAt: time.Now()},
	}
	out := renderList(buildItems(sessions, nil, nil), 0, 120, 10)
	require.Contains(t, out, "claude", "an empty backend must render as claude")
}

func TestBackendOrDefaultsToClaude(t *testing.T) {
	require.Equal(t, "claude", backendOr(&store.Session{}), "empty backend → claude")
	require.Equal(t, "aider", backendOr(&store.Session{Backend: "aider"}), "explicit backend preserved")
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
	out := renderList(buildItems(sessions, nil, nil), 0, 30, 10)
	require.Contains(t, out, "agent-abc")
	require.Contains(t, out, "88k")
	require.Contains(t, out, "2m")
}

func TestRenderListClampsToHeightAndKeepsCursor(t *testing.T) {
	var sessions []*store.Session
	for i := 0; i < 20; i++ {
		sessions = append(sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	out := renderList(buildItems(sessions, nil, nil), 18, 80, 8)
	require.Len(t, strings.Split(out, "\n"), 8, "rendered to exactly height lines")
	require.Contains(t, out, "agent-18", "the selected row is within the window")
	require.Contains(t, out, "more", "a ▲/▼ hint appears when rows are hidden")
}

func TestRenderListShortListPadsToHeight(t *testing.T) {
	sessions := []*store.Session{{ID: "only", Status: store.StatusWorking}}
	require.Len(t, strings.Split(renderList(buildItems(sessions, nil, nil), 0, 80, 6), "\n"), 6, "short list padded to height")
}

func TestRenderListHeightOneRendersSingleLine(t *testing.T) {
	var sessions []*store.Session
	for i := 0; i < 5; i++ {
		sessions = append(sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	require.Len(t, strings.Split(renderList(buildItems(sessions, nil, nil), 0, 80, 1), "\n"), 1, "height 1 with many rows still renders exactly 1 line")
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

// Ordering keys on CreatedAt (immutable), not UpdatedAt, so rows stay put as
// agents work. Directories are chronological by their FIRST (oldest) agent, and
// agents within a directory are oldest-first — so creating an agent never re-sorts
// its directory. Here UpdatedAt is deliberately scrambled relative to CreatedAt to
// prove it has no effect on order: group /a's oldest agent (a2) predates group /b's
// oldest (b2), so /a sorts first, and within each group the oldest agent leads.
func TestGroupSortOrdersGroupsByCreationAndIgnoresUpdatedAt(t *testing.T) {
	now := time.Now()
	in := []*store.Session{
		{ID: "a2", Workdir: "/a", CreatedAt: now.Add(-4 * time.Minute), UpdatedAt: now},
		{ID: "b2", Workdir: "/b", CreatedAt: now.Add(-3 * time.Minute), UpdatedAt: now.Add(-9 * time.Minute)},
		{ID: "a1", Workdir: "/a", CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-8 * time.Minute)},
		{ID: "b1", Workdir: "/b", CreatedAt: now.Add(-1 * time.Minute), UpdatedAt: now.Add(-7 * time.Minute)},
	}
	out := groupSort(in)
	got := []string{out[0].ID, out[1].ID, out[2].ID, out[3].ID}
	require.Equal(t, []string{"a2", "a1", "b2", "b1"}, got)
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
	out := renderList(buildItems(sessions, nil, nil), 0, 120, 12)
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
		{ID: "a1", Workdir: "/a", CreatedAt: now},
		{ID: "a2", Workdir: "/a", CreatedAt: now.Add(-time.Minute)},
		{ID: "b1", Workdir: "/b", CreatedAt: now.Add(-2 * time.Minute)},
	})
	items := buildItems(ss, nil, nil)
	require.Len(t, items, 3)
	// /b sorts first (its oldest agent b1 predates /a's oldest a2); within /a the
	// oldest agent (a2) leads.
	require.Equal(t, "b1", items[0].session.ID)
	require.Equal(t, "a2", items[1].session.ID)
	require.Equal(t, "a1", items[2].session.ID)
	require.Equal(t, "/b", items[0].dir)
}

// A spawned child nests directly under its parent, indented, and the parent
// becomes a collapsible header.
func TestBuildItemsNestsChildUnderParent(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "parent", Workdir: "/a", Status: store.StatusWorking, CreatedAt: now},
		{ID: "child", ParentID: "parent", Workdir: "/a", Status: store.StatusWorking, CreatedAt: now.Add(-time.Minute)},
	}
	items := buildItems(ss, nil, nil)
	require.Len(t, items, 2)
	require.Equal(t, "parent", items[0].session.ID)
	require.True(t, items[0].hasKids, "parent marked as a collapsible header")
	require.Equal(t, 0, items[0].depth)
	require.Equal(t, "child", items[1].session.ID)
	require.Equal(t, 1, items[1].depth, "child indented one level")
	require.Equal(t, "/a", items[1].dir, "child inherits the root's dir group")
}

// Collapsing a parent hides its entire sub-tree (children and grandchildren).
func TestBuildItemsCollapsedParentHidesSubtree(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "a", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now},
		{ID: "b", ParentID: "a", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now.Add(-time.Minute)},
		{ID: "c", ParentID: "b", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now.Add(-2 * time.Minute)},
	}
	items := buildItems(ss, nil, map[string]bool{"a": true})
	require.Len(t, items, 1, "collapsed root hides its whole sub-tree")
	require.Equal(t, "a", items[0].session.ID)
	require.True(t, items[0].collapsed)
	require.True(t, items[0].hasKids)
}

// Deep nesting A→B→C indents each level in DFS pre-order.
func TestBuildItemsDeepNestingDepths(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "a", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now},
		{ID: "b", ParentID: "a", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now.Add(-time.Minute)},
		{ID: "c", ParentID: "b", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now.Add(-2 * time.Minute)},
	}
	items := buildItems(ss, nil, nil)
	require.Len(t, items, 3)
	require.Equal(t, []string{"a", "b", "c"}, []string{items[0].session.ID, items[1].session.ID, items[2].session.ID})
	require.Equal(t, []int{0, 1, 2}, []int{items[0].depth, items[1].depth, items[2].depth})
}

// A terminal parent that still anchors a live child renders as a tombstone with
// the running-descendant count, and carries no live badge.
func TestBuildItemsTombstoneParentWithLiveChild(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "gone", Workdir: "/w", Status: store.StatusDone, CreatedAt: now},
		{ID: "live", ParentID: "gone", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now.Add(-time.Minute)},
	}
	items := buildItems(ss, nil, nil)
	require.Len(t, items, 2)
	require.True(t, items[0].tombstone, "terminal parent with children is a tombstone")
	require.Equal(t, 1, items[0].runningKids)

	out := renderItemLine(items[0], false, 80)
	require.Contains(t, out, "(terminated · 1 running)")
	require.NotContains(t, out, "done", "tombstone shows no live status badge")
}

// The cursor highlight must reach the whole selected row — including agents with
// no name. A blank name used to be rendered as a styled "—", which embedded an
// ANSI reset at the very start of the line and cut the cursor highlight off before
// the agent id; a named agent (plain leading text) stayed highlighted. Force a
// color profile so Render emits real SGR codes, then assert no reset appears
// before the agent id on either row.
func TestRenderItemLineSelectedHighlightsUnnamedAgent(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	// The id is highlighted iff the cursor's SGR (bold cyan) is the active style
	// right before it — i.e. the last cursor-SGR before the id is not undone by a
	// reset between them. (The "› " caret emits its own trailing reset, so a plain
	// "no reset before id" check would wrongly fail for every row.)
	const cursorSGR = "\x1b[1;36m"
	highlightedThroughID := func(id string, s *store.Session) bool {
		out := renderItemLine(item{session: s}, true, 80)
		i := strings.Index(out, id)
		require.GreaterOrEqual(t, i, 0, "agent id must render")
		lastSet := strings.LastIndex(out[:i], cursorSGR)
		if lastSet < 0 {
			return false // cursor style never (re)applied before the id
		}
		return !strings.Contains(out[lastSet+len(cursorSGR):i], "\x1b[0m")
	}

	require.True(t, highlightedThroughID("u1", &store.Session{ID: "u1", Status: store.StatusWorking}),
		"unnamed selected agent must stay highlighted through its id")
	require.True(t, highlightedThroughID("n1", &store.Session{ID: "n1", Name: "named", Status: store.StatusWorking}),
		"named selected agent stays highlighted (parity check)")
}

// An orphan child (its parent id is not in the set) is promoted to a root rather
// than vanishing.
func TestBuildItemsOrphanChildPromotedToRoot(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "orphan", ParentID: "missing", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now},
	}
	items := buildItems(ss, nil, nil)
	require.Len(t, items, 1)
	require.Equal(t, "orphan", items[0].session.ID)
	require.Equal(t, 0, items[0].depth, "orphan rendered as a root")
	require.False(t, items[0].hasKids)
}

// A parent cycle (a→b→a) never loops forever and emits each node once.
func TestBuildItemsParentCycleGuarded(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{
		{ID: "a", ParentID: "b", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now},
		{ID: "b", ParentID: "a", Workdir: "/w", Status: store.StatusWorking, CreatedAt: now.Add(-time.Minute)},
	}
	items := buildItems(ss, nil, nil)
	// Neither is a root (each parent is present), so the forest is anchored only by
	// the cycle guard; the build must terminate and not duplicate a node.
	seen := map[string]int{}
	for _, it := range items {
		if it.session != nil {
			seen[it.session.ID]++
		}
	}
	for id, n := range seen {
		require.Equalf(t, 1, n, "node %s emitted exactly once", id)
	}
}

// liveStatus mirrors the daemon: running/awaiting states are live, terminal ones
// are not.
func TestLiveStatus(t *testing.T) {
	for _, s := range []store.Status{store.StatusSpawning, store.StatusWorking, store.StatusWaitingForInput, store.StatusIdle} {
		require.Truef(t, liveStatus(s), "%s is live", s)
	}
	for _, s := range []store.Status{store.StatusDone, store.StatusErrored, store.StatusOrphaned, store.StatusRateLimited} {
		require.Falsef(t, liveStatus(s), "%s is terminal", s)
	}
}

// treePrefix indents by depth and switches the glyph on collapse; a childless
// root gets no prefix (flat list unchanged).
func TestTreePrefix(t *testing.T) {
	require.Equal(t, "", treePrefix(item{depth: 0}))
	require.Equal(t, "▾ ", treePrefix(item{depth: 0, hasKids: true}))
	require.Equal(t, "▸ ", treePrefix(item{depth: 0, hasKids: true, collapsed: true}))
	require.Equal(t, "      ", treePrefix(item{depth: 2}), "leaf at depth 2: 4 indent + 2 align = 6 spaces")
}

func TestBuildItemsEmptyOpenedDirSortsChronologically(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{{ID: "a1", Workdir: "/a", CreatedAt: now.Add(-time.Hour)}}
	opened := map[string]time.Time{"/freshly/opened": now} // newer than the agent
	items := buildItems(ss, opened, nil)
	require.Len(t, items, 2, "one agent + one placeholder")
	// Chronological order: the older /a agent leads, the freshly-opened dir's
	// placeholder sorts to the bottom as the most recent entry.
	require.Equal(t, "a1", items[0].session.ID)
	require.Nil(t, items[1].session, "placeholder is last (most recent)")
	require.Equal(t, "/freshly/opened", items[1].dir)
}

func TestBuildItemsOpenedDirWithAgentsHasNoPlaceholder(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{{ID: "a1", Workdir: "/a", UpdatedAt: now}}
	opened := map[string]time.Time{"/a": now.Add(-time.Hour)} // /a already has an agent
	items := buildItems(ss, opened, nil)
	require.Len(t, items, 1, "no placeholder for an opened dir that has agents")
	require.Equal(t, "a1", items[0].session.ID)
}

func TestBuildItemsOnlyOpenedDirsNoSessions(t *testing.T) {
	now := time.Now()
	items := buildItems(nil, map[string]time.Time{"/x": now}, nil)
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
		nil,
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
		out := renderList(buildItems(sessions, nil, nil), 5, 80, h)
		require.Len(t, strings.Split(out, "\n"), h, "exactly height lines at h=%d", h)
		require.Contains(t, out, "a5", "selected agent must stay visible at h=%d", h)
	}
}

func TestPipelineItems(t *testing.T) {
	ps := []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}, {ID: "b", Status: pipeline.JobPending}}}}
	items := pipelineItems(ps, nil, nil)
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

func TestRenderItemLineJobRowWithLiveSession(t *testing.T) {
	// A running job linked to a live session surfaces the agent badge, the token
	// gauge, and the branch.
	job := &pipeline.Job{ID: "implement", Status: pipeline.JobRunning, Branch: "feat/x"}
	sess := &store.Session{
		ID:            "agent-1",
		Status:        store.StatusWaitingForInput,
		ContextTokens: 120_000,
		ContextState:  store.ContextWarning,
	}
	row := renderItemLine(item{pjPipe: "demo", pjJob: job, pjSess: sess}, false, 100)
	for _, want := range []string{"implement", "running", "needs-input", "120k", "feat/x"} {
		if !strings.Contains(row, want) {
			t.Fatalf("job row missing %q: %q", want, row)
		}
	}

	// No job branch → fall back to the session's worktree basename.
	job2 := &pipeline.Job{ID: "build", Status: pipeline.JobRunning}
	sess2 := &store.Session{ID: "agent-2", Status: store.StatusWorking, Worktree: "/home/u/repo/.worktrees/PROJ-9"}
	row = renderItemLine(item{pjPipe: "demo", pjJob: job2, pjSess: sess2}, false, 100)
	if !strings.Contains(row, "PROJ-9") {
		t.Fatalf("job row should show worktree basename: %q", row)
	}

	// No linked session → lean row, no panic, still shows id + status.
	row = renderItemLine(item{pjPipe: "demo", pjJob: &pipeline.Job{ID: "pending", Status: pipeline.JobPending}}, false, 100)
	if !strings.Contains(row, "pending") {
		t.Fatalf("session-less job row wrong: %q", row)
	}
}

func TestPipelineItemsLinksSessions(t *testing.T) {
	ps := []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning, SessionID: "agent-1"}, {ID: "b", Status: pipeline.JobPending}}}}
	sessions := []*store.Session{{ID: "agent-1", Status: store.StatusWorking}}
	items := pipelineItems(ps, sessions, nil)
	if items[1].pjSess == nil || items[1].pjSess.ID != "agent-1" {
		t.Fatalf("job a should link session agent-1: %+v", items[1].pjSess)
	}
	if items[2].pjSess != nil {
		t.Fatalf("job b has no session, should stay nil: %+v", items[2].pjSess)
	}
}

func TestRenderItemLineAgentWithBranchAndWorktree(t *testing.T) {
	// Agent with worktree — should show worktree name in brackets
	sWithWorktree := &store.Session{
		ID:       "agent-123",
		Status:   store.StatusWorking,
		Worktree: "/home/user/repo/.worktrees/PROJ-123",
		Branch:   "feature/proj-123",
	}
	row := renderItemLine(item{session: sWithWorktree}, false, 80)
	if !strings.Contains(row, "agent-123") {
		t.Errorf("row missing agent ID: %q", row)
	}
	if !strings.Contains(row, "PROJ-123") {
		t.Errorf("row should show worktree name PROJ-123: %q", row)
	}

	// Agent with only branch — should show branch name in brackets
	sWithBranch := &store.Session{
		ID:     "agent-456",
		Status: store.StatusWorking,
		Branch: "fix/auth-bug",
	}
	row = renderItemLine(item{session: sWithBranch}, false, 80)
	if !strings.Contains(row, "fix/auth-bug") {
		t.Errorf("row should show branch name: %q", row)
	}

	// Agent with neither — should not have brackets
	sPlain := &store.Session{
		ID:     "agent-789",
		Status: store.StatusWorking,
	}
	row = renderItemLine(item{session: sPlain}, false, 80)
	if !strings.Contains(row, "agent-789") {
		t.Errorf("row missing agent ID: %q", row)
	}
	// The row should still render cleanly without crashing
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

func pipeModel() controlPaneModel {
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
	fa := updated.(controlPaneModel).api.(*fakeAPI)
	if fa.canceled != "demo" {
		t.Fatalf("want canceled=demo, got %q", fa.canceled)
	}
}

func TestKeyPauseResumePipeline(t *testing.T) {
	// running pipeline → p pauses it.
	m := pipeModel()
	m.cursor = 0 // the pipeline header row
	updated, cmd := m.handleKey(key("p"))
	if cmd == nil {
		t.Fatalf("p on a running pipeline should return a pause cmd")
	}
	cmd()
	fa := updated.(controlPaneModel).api.(*fakeAPI)
	if fa.paused != "demo" {
		t.Fatalf("want paused=demo, got %q", fa.paused)
	}

	// paused pipeline → p resumes it.
	m = pipeModel()
	m.pipelines[0].Status = pipeline.StatusPaused
	m.cursor = 0
	updated, cmd = m.handleKey(key("p"))
	if cmd == nil {
		t.Fatalf("p on a paused pipeline should return a resume cmd")
	}
	cmd()
	fa = updated.(controlPaneModel).api.(*fakeAPI)
	if fa.resumed != "demo" {
		t.Fatalf("want resumed=demo, got %q", fa.resumed)
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

func TestKeyInfoOnPipelineJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 1 // job "a"
	updated, _ := m.handleKey(key("i"))
	um := updated.(controlPaneModel)
	if um.mode != modeDetails {
		t.Fatalf("i on a job row should open modeDetails, got %v", um.mode)
	}
	if got := um.detailTitle(); got != "demo/a" {
		t.Fatalf("detail title want demo/a, got %q", got)
	}
}

func TestJobDetailBody(t *testing.T) {
	job := &pipeline.Job{ID: "a", Status: pipeline.JobFailed, Prompt: "do the thing"}
	// no session → the job's stored detail (prompt visible).
	body := jobDetailBody(job, nil, 80)
	if !strings.Contains(body, "do the thing") {
		t.Fatalf("session-less job detail should show stored job detail: %q", body)
	}
	// live session → the agent's detail (session id visible).
	sess := &store.Session{ID: "demo-a", Status: store.StatusWorking}
	body = jobDetailBody(job, sess, 80)
	if !strings.Contains(body, "demo-a") {
		t.Fatalf("live-session job detail should show agent detail: %q", body)
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
		{"paused", &pipeline.Pipeline{Status: pipeline.StatusPaused, Jobs: []pipeline.Job{job(pipeline.JobRunning)}}, "paused", "⏸", lipgloss.Color("3")},
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
	items := pipelineItems(ps, nil, map[string]bool{"demo": true})
	require.Len(t, items, 1, "collapsed pipeline emits only its header row")
	require.NotNil(t, items[0].pipeline)
	require.True(t, items[0].collapsed, "header item carries collapsed state")

	// expanded (explicit false) → header + jobs.
	items = pipelineItems(ps, nil, map[string]bool{"demo": false})
	require.Len(t, items, 3, "expanded pipeline emits header + job rows")
	require.False(t, items[0].collapsed)
}

func TestKeyCollapseExpandPipeline(t *testing.T) {
	m := pipeModel()
	m.cursor = 0 // pipeline header

	// collapse with h
	updated, _ := m.handleKey(key("h"))
	mc := updated.(controlPaneModel)
	require.True(t, mc.collapsed["demo"], "h collapses the pipeline under the cursor")
	require.Len(t, mc.items(), 1, "collapsed → only the header remains")

	// expand with l
	updated, _ = mc.handleKey(key("l"))
	me := updated.(controlPaneModel)
	require.False(t, me.collapsed["demo"], "l expands the pipeline under the cursor")
	require.Len(t, me.items(), 3, "expanded → header + 2 jobs")
}

func TestKeyCollapseFromJobRepinsCursorToHeader(t *testing.T) {
	m := pipeModel()
	m.cursor = 1 // job "a" (a hidden row once collapsed)

	updated, _ := m.handleKey(key("h"))
	mc := updated.(controlPaneModel)
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

func TestContextLabel(t *testing.T) {
	cases := []struct {
		tokens int
		state  string
		want   string
	}{
		{0, "", ""},
		{145000, "ok", "145k"},
		{210000, "warning", "210k"},
		{410000, "critical", "410k"},
	}
	for _, c := range cases {
		if got, _ := contextLabel(c.tokens, c.state); got != c.want {
			t.Errorf("contextLabel(%d,%q)=%q, want %q", c.tokens, c.state, got, c.want)
		}
	}
}
