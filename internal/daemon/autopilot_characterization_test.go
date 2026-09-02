package daemon

// WP1 characterization: autopilot manager tombstone behavior when workers remain
// parented to a rotated/deleted manager. Seam inventory:
// docs/specs/2026-09-01-autopilot-plan-scoped-hierarchy-wp1-seams.md

import (
	"net/http"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// TestCharacterization_AutopilotRotatedManagerTombstonesWithLiveWorkers freezes
// the production consequence from spec §2.3: deleting an autopilot manager that
// still has live MCP workers tombstones the record (strict_lifecycle.go:288-311)
// instead of reaping it. WP6 stops parenting workers so rotated managers reap.
func TestCharacterization_AutopilotRotatedManagerTombstonesWithLiveWorkers(t *testing.T) {
	fs := newFakeStore()
	fs.data["agent-8bed0ec5"] = &store.Session{
		ID:          "agent-8bed0ec5",
		TmuxSession: "agent-8bed0ec5",
		Role:        "autopilot",
		Status:      store.StatusWorking,
		Tags:        []string{"autopilot", "run:ap-deadbeef1234"},
	}
	fs.data["worker-task-a"] = &store.Session{
		ID:       "worker-task-a",
		ParentID: "agent-8bed0ec5",
		Status:   store.StatusWorking,
		Tags:     []string{"autopilot", "run:ap-deadbeef1234"},
	}
	fl := &fakeLife{}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	code, out := deleteSession(t, ts, "agent-8bed0ec5", false)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "tombstoned", out.Status)

	got, ok := fs.data["agent-8bed0ec5"]
	require.True(t, ok, "rotated manager stays as an active tombstone record")
	require.Equal(t, store.StatusDone, got.Status)
	require.Contains(t, fs.data, "worker-task-a", "worker remains parented to tombstoned manager")
	require.Equal(t, "agent-8bed0ec5", fs.data["worker-task-a"].ParentID,
		"workers keep parent_id to the dead manager; WP6 clears this for autopilot slots")
}
