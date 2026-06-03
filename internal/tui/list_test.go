package tui

import (
	"fmt"
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
