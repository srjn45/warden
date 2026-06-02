package tui

import (
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
	out := m.renderList(120)
	require.Contains(t, out, "<1m", "renderList output should contain the age token <1m")
	// Ensure the subject is still present too.
	require.True(t, strings.Contains(out, "test subject") || strings.Contains(out, "test subjec"),
		"renderList output should contain (possibly truncated) subject")
}
