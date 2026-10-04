package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/stretchr/testify/require"
)

// Exercise the upgrade through the same handlers used by history, restore,
// attach, and teardown, with a real persisted archive and an independent shell.
func TestArchivedAgentLifecycleAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	legacy, err := store.NewFileStore(dir)
	require.NoError(t, err)
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "old-agent", Name: "archived-worker", Status: store.StatusOrphaned,
		AICLISessionID: "resume-123", TmuxSession: "old-agent", Workdir: t.TempDir(),
	}))
	require.NoError(t, legacy.Archive(ctx, "old-agent"))
	require.NoError(t, legacy.Insert(ctx, &store.Session{
		ID: "shell", Name: "n-shell", Kind: store.KindTerminal, TmuxSession: "shell", Status: store.StatusWorking,
	}))
	require.NoError(t, legacy.Close(ctx))

	agents, err := agentstore.New(dir)
	require.NoError(t, err)
	require.NoError(t, agents.Close())
	agents, err = agentstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = agents.Close() })

	terms, err := terminalstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = terms.Close() })

	life := &fakeLife{}
	srv := NewServer(agents, life, nil, time.Second, true, nil, nil, nil)
	srv.SetTerminals(terms)

	history, err := srv.ListHistory(ctx, oapi.ListHistoryRequestObject{})
	require.NoError(t, err)
	rows := history.(oapi.ListHistory200JSONResponse).Sessions
	require.Len(t, rows, 1)
	require.Equal(t, "old-agent", rows[0].ID)

	_, err = srv.RestoreSession(ctx, oapi.RestoreSessionRequestObject{Id: "archived-worker"})
	require.NoError(t, err)
	require.Equal(t, "old-agent", life.restored)

	restored, err := agents.Get(ctx, "old-agent")
	require.NoError(t, err)
	require.Equal(t, "resume-123", restored.AICLISessionID)
	require.Equal(t, store.StatusSpawning, restored.Status)

	// The attach handler resolves this DTO before handing tmux to the PTY bridge.
	attached, err := srv.resolveSessionDTO(ctx, "archived-worker")
	require.NoError(t, err)
	require.Equal(t, "old-agent", attached.TmuxSession)

	_, err = srv.TerminateSession(ctx, oapi.TerminateSessionRequestObject{Id: "old-agent"})
	require.NoError(t, err)
	_, err = srv.DeleteSession(ctx, oapi.DeleteSessionRequestObject{Id: "old-agent"})
	require.NoError(t, err)
	_, err = agents.Get(ctx, "old-agent")
	require.ErrorIs(t, err, agentstore.ErrNotFound)

	shell, err := terms.Get(ctx, "shell")
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusRunning, shell.Status)
	_, err = agents.Get(ctx, "shell")
	require.ErrorIs(t, err, agentstore.ErrNotFound)

	// Agent-only recovery and approval endpoints cannot resolve the shell.
	_, err = srv.resolveSession(ctx, "shell")
	require.Error(t, err)
	shellDTO, err := srv.resolveSessionDTO(ctx, "shell")
	require.NoError(t, err)
	require.Equal(t, store.KindTerminal, shellDTO.Kind)
}
