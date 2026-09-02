package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type slotSpawnStore struct {
	*fakeStore
}

func TestSpawnBrainCreatesManagerSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl}
	rt := autopilotRuntime{s: srv}

	spec := autopilot.BrainSpec{
		SlotScope: "voyage",
		Repo:      repo,
		Prompt:    "brief",
		Backend:   "claude",
		Tags:      []string{"autopilot", "run:ap-abc"},
	}
	handle, err := rt.SpawnBrain(context.Background(), spec)
	require.NoError(t, err)
	require.Equal(t, "voyage-autopilot", handle.AgentID)
	require.NotNil(t, fl.spawned)
	require.Equal(t, "voyage-autopilot", fl.spawned.Ticket)
}

func TestSpawnBrainAdoptsLiveSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	now := time.Now().UTC()
	fs.data["voyage-autopilot"] = &store.Session{
		ID: "voyage-autopilot", TmuxSession: "voyage-autopilot",
		Status: store.StatusWorking, Backend: "claude", UpdatedAt: now, CreatedAt: now,
	}
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl}
	rt := autopilotRuntime{s: srv}

	handle, err := rt.SpawnBrain(context.Background(), autopilot.BrainSpec{
		SlotScope: "voyage",
		Repo:      repo,
		Prompt:    "brief",
		Tags:      []string{"autopilot", "run:ap-abc"},
	})
	require.NoError(t, err)
	require.Equal(t, "voyage-autopilot", handle.AgentID)
	require.Nil(t, fl.spawned, "live slot is adopted without spawning")
}

func TestSpawnGuardianCreatesAndAdoptsSlot(t *testing.T) {
	repo := t.TempDir()
	fs := &slotSpawnStore{fakeStore: newFakeStore()}
	srv := &Server{store: fs}
	rt := autopilotRuntime{s: srv}
	ctx := context.Background()

	id, err := rt.SpawnGuardian(ctx, "ap-abc", "voyage", repo)
	require.NoError(t, err)
	require.Equal(t, "voyage-guardian", id)

	id2, err := rt.SpawnGuardian(ctx, "ap-abc", "voyage", repo)
	require.NoError(t, err)
	require.Equal(t, "voyage-guardian", id2)
	require.Len(t, fs.data, 1)
}
