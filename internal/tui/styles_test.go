package tui

import (
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestBadgeErroredProjectsExitEvidence(t *testing.T) {
	code := 137
	label, _ := badge(store.StatusErrored, &code)
	require.Equal(t, "done", label)

	label2, _ := badge(store.StatusErrored, nil)
	require.Equal(t, "orphaned", label2)

	label3, _ := badge(store.StatusDone, nil)
	require.Equal(t, "done", label3)
}

func TestBadgeSevenStates(t *testing.T) {
	for _, tt := range []struct {
		status store.Status
		want   string
	}{
		{store.StatusSpawning, "pending"}, {store.StatusWorking, "busy"},
		{store.StatusWaitingForInput, "need-input"}, {store.StatusIdle, "idle"},
		{store.StatusDone, "done"}, {store.StatusOrphaned, "orphaned"},
		{store.StatusRateLimited, "rate_limited"},
	} {
		label, _ := badge(tt.status, nil)
		require.Equal(t, tt.want, label)
	}
}
