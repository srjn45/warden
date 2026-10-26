package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestRetentionSweepPurgesExpiredArchivesAndInboxes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	agents, err := agentstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = agents.Close() })
	mbox, err := mailbox.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mbox.Close() })

	for _, id := range []string{"live", "old"} {
		require.NoError(t, agents.Insert(ctx, &agentstore.Agent{ID: id, Name: id, Status: store.StatusIdle}))
	}
	require.NoError(t, agents.Archive(ctx, "old"))
	for _, to := range []string{"live", "old", "ghost", "human"} {
		_, err := mbox.Append(mailbox.Message{To: to, From: "daemon", Body: "hi"})
		require.NoError(t, err)
	}

	srv := NewServer(agents, &fakeLife{}, nil, time.Second, true, nil, mbox, nil)
	srv.SetRetention(24*time.Hour, 0, 7)

	require.Zero(t, srv.retentionSweep(ctx, time.Now()), "inside the restore window")
	closed, err := agents.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)

	require.Equal(t, 1, srv.retentionSweep(ctx, time.Now().Add(25*time.Hour)))
	closed, err = agents.ListClosed(ctx)
	require.NoError(t, err)
	require.Empty(t, closed)
	_, err = agents.Get(ctx, "live")
	require.NoError(t, err, "live agents are never purged")

	for to, want := range map[string]int{"live": 1, "human": 1, "old": 0, "ghost": 0} {
		got, err := mbox.Messages(to)
		require.NoError(t, err)
		require.Len(t, got, want, to)
	}
}

func TestRetentionSweepOffKeepsArchives(t *testing.T) {
	ctx := context.Background()
	agents, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = agents.Close() })
	require.NoError(t, agents.Insert(ctx, &agentstore.Agent{ID: "old", Name: "old", Status: store.StatusIdle}))
	require.NoError(t, agents.Archive(ctx, "old"))

	srv := NewServer(agents, &fakeLife{}, nil, time.Second, true, nil, nil, nil)
	srv.SetRetention(0, 0, 7)
	require.Zero(t, srv.retentionSweep(ctx, time.Now().Add(365*24*time.Hour)))
	closed, err := agents.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
}
