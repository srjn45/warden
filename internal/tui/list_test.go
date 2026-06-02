package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

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
