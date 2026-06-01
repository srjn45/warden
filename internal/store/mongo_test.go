package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
)

func newTestStore(t *testing.T) *MongoStore {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping mongo integration test in -short mode")
	}
	ctx := context.Background()
	container, err := mongodb.Run(ctx, "mongo:7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	uri, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	st, err := NewMongoStore(ctx, uri, "agentctl_test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close(ctx) })
	return st
}

func sample() *Session {
	return &Session{
		ID: "PROJ-350", Ticket: "PROJ-350", TmuxSession: "PROJ-350",
		Repo: "/repo", Worktree: ".worktrees/PROJ-350", Branch: "PROJ-350",
		Status: StatusSpawning,
	}
}

func TestInsertGet(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))

	got, err := st.Get(ctx, "PROJ-350")
	require.NoError(t, err)
	require.Equal(t, StatusSpawning, got.Status)
	require.False(t, got.CreatedAt.IsZero(), "Insert must stamp CreatedAt")
}

func TestInsertDuplicate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.ErrorIs(t, st.Insert(ctx, sample()), ErrExists)
}

func TestGetNotFound(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	_, err := st.Get(ctx, "nope")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestUpdateStatusBumpsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	before, _ := st.Get(ctx, "PROJ-350")
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, st.UpdateStatus(ctx, "PROJ-350", StatusWorking))
	after, _ := st.Get(ctx, "PROJ-350")
	require.Equal(t, StatusWorking, after.Status)
	require.True(t, after.UpdatedAt.After(before.UpdatedAt))
}

func TestAppendEvent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.AppendEvent(ctx, "PROJ-350", Event{Type: "Notification", Detail: "Allow Bash?"}))
	got, _ := st.Get(ctx, "PROJ-350")
	require.Len(t, got.Events, 1)
	require.Equal(t, "Notification", got.Events[0].Type)
	require.False(t, got.Events[0].TS.IsZero(), "AppendEvent must stamp ts when zero")
}

func TestArchiveRemovesFromActive(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	require.NoError(t, st.Archive(ctx, "PROJ-350"))
	_, err := st.Get(ctx, "PROJ-350")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestList(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	require.NoError(t, st.Insert(ctx, sample()))
	s2 := sample()
	s2.ID, s2.Ticket = "PROJ-343", "PROJ-343"
	require.NoError(t, st.Insert(ctx, s2))
	all, err := st.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
}
