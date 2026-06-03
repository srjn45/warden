package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
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
	m := New(&fakeAPI{})
	m.sessions = []*store.Session{
		{
			ID:        "agent-abc",
			Status:    store.StatusWorking,
			UpdatedAt: time.Now().Add(-30 * time.Second), // 30s ago → "<1m"
			Subject:   "test subject",
		},
	}
	out := renderList(m.sessions, m.cursor, 120, 10)
	require.Contains(t, out, "<1m", "renderList output should contain the age token <1m")
	// Ensure the subject is still present too.
	require.True(t, strings.Contains(out, "test subject") || strings.Contains(out, "test subjec"),
		"renderList output should contain (possibly truncated) subject")
}

func TestRenderListClampsToHeightAndKeepsCursor(t *testing.T) {
	m := New(&fakeAPI{})
	for i := 0; i < 20; i++ {
		m.sessions = append(m.sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	m.cursor = 18
	out := renderList(m.sessions, m.cursor, 80, 8)
	require.Len(t, strings.Split(out, "\n"), 8, "rendered to exactly height lines")
	require.Contains(t, out, "agent-18", "the selected row is within the window")
	require.Contains(t, out, "more", "a ▲/▼ hint appears when rows are hidden")
}

func TestRenderListShortListPadsToHeight(t *testing.T) {
	m := New(&fakeAPI{})
	m.sessions = []*store.Session{{ID: "only", Status: store.StatusWorking}}
	require.Len(t, strings.Split(renderList(m.sessions, m.cursor, 80, 6), "\n"), 6, "short list padded to height")
}

func TestRenderListHeightOneRendersSingleLine(t *testing.T) {
	m := New(&fakeAPI{})
	for i := 0; i < 5; i++ {
		m.sessions = append(m.sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	require.Len(t, strings.Split(renderList(m.sessions, m.cursor, 80, 1), "\n"), 1, "height 1 with many rows still renders exactly 1 line")
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
	m := New(&fakeAPI{})
	m.sessions = groupSort([]*store.Session{
		{ID: "a1", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: time.Now()},
		{ID: "a2", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: time.Now().Add(-time.Minute)},
		{ID: "b1", Workdir: "/work/beta", Status: store.StatusWorking, UpdatedAt: time.Now().Add(-2 * time.Minute)},
	})
	out := renderList(m.sessions, m.cursor, 120, 12)
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

func TestItemKeyDistinguishesAgentsFromPlaceholders(t *testing.T) {
	require.Equal(t, "agent-x", itemKey(item{session: &store.Session{ID: "agent-x"}, dir: "/a"}))
	require.Equal(t, dirKey("/a"), itemKey(item{dir: "/a"}))
	require.NotEqual(t, itemKey(item{dir: "/a"}), itemKey(item{session: &store.Session{ID: "/a"}, dir: "/a"}))
}

func TestRenderListGroupedSmallHeightKeepsCursor(t *testing.T) {
	m := New(&fakeAPI{})
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
	m.sessions = groupSort(ss)
	m.cursor = 5
	for h := 1; h <= 4; h++ {
		out := renderList(m.sessions, m.cursor, 80, h)
		require.Len(t, strings.Split(out, "\n"), h, "exactly height lines at h=%d", h)
		require.Contains(t, out, "a5", "selected agent must stay visible at h=%d", h)
	}
}
