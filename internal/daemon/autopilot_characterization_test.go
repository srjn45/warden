package daemon

// WP6: autopilot workers use run back-refs instead of parent_id, so deleting a
// manager with live workers no longer tombstones; dead managers reap when the
// run's workers finish.

import (
	"net/http"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestAutopilotManagerDeleteWithLiveWorkersNoTombstone(t *testing.T) {
	fs := newFakeStore()
	fs.data["agent-8bed0ec5"] = &store.Session{
		ID:          "agent-8bed0ec5",
		TmuxSession: "agent-8bed0ec5",
		Role:        "autopilot",
		Status:      store.StatusWorking,
		Tags:        []string{"autopilot", "run:ap-deadbeef1234"},
	}
	fs.data["worker-task-a"] = &store.Session{
		ID:             "worker-task-a",
		Status:         store.StatusWorking,
		AutopilotRunID: "ap-deadbeef1234",
		AutopilotSlot:  store.AutopilotSlotWorker,
		Tags:           []string{"autopilot", "run:ap-deadbeef1234"},
	}
	fl := &fakeLife{}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	code, out := deleteSession(t, ts, "agent-8bed0ec5", false)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "deleted", out.Status)
	require.NotContains(t, fs.data, "agent-8bed0ec5", "manager is archived, not tombstoned")
	require.Contains(t, fs.data, "worker-task-a")
	require.Empty(t, fs.data["worker-task-a"].ParentID)
}

func TestReapAutopilotManagerWhenLastWorkerFinishes(t *testing.T) {
	fs := newFakeStore()
	fs.data["agent-old-brain"] = &store.Session{
		ID: "agent-old-brain", Role: "autopilot", Status: store.StatusDone,
		Tags: []string{"autopilot", "run:ap-deadbeef1234"},
	}
	fs.data["worker-task-a"] = &store.Session{
		ID: "worker-task-a", Status: store.StatusWorking,
		AutopilotRunID: "ap-deadbeef1234", AutopilotSlot: store.AutopilotSlotWorker,
		Tags: []string{"autopilot", "run:ap-deadbeef1234"},
	}
	d := &pollerDeps{store: fs}

	swapped, err := d.FinalizeExit(t.Context(), "worker-task-a", store.StatusWorking, store.StatusDone, 0)
	require.NoError(t, err)
	require.True(t, swapped)
	require.NotContains(t, fs.data, "agent-old-brain", "terminal manager reaps once workers finish")
}
