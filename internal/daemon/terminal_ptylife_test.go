package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/terminalstore"
)

func spawnedTerminal(t *testing.T, srv *Server, cwd string) string {
	t.Helper()
	resp, err := srv.SpawnAgent(context.Background(), spawnTerminalReq(cwd, ""))
	require.NoError(t, err)
	return resp.(oapi.SpawnAgent201JSONResponse).ID
}

// F-LIFE-04 / G12: a direct shell that exits with a status is recorded as
// exited with that code; a signal or lost process stays orphaned and
// restorable. The end is recorded once (compare-and-swap).
func TestDirectTerminalExitRecordsStatus(t *testing.T) {
	ctx := context.Background()
	srv, fl, _, ts := terminalGuardServer(t)
	id := spawnedTerminal(t, srv, t.TempDir())

	three := 3
	srv.recordDirectTerminalEnd(id, &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, ExitCode: &three})
	got, err := ts.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusExited, got.Status)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, 3, *got.ExitCode)

	// A second, later event for the same end is a no-op.
	zero := 0
	srv.recordDirectTerminalEnd(id, &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, ExitCode: &zero})
	got, _ = ts.Get(ctx, id)
	require.Equal(t, 3, *got.ExitCode)

	// An exited shell can be relaunched through the public restore command.
	_, err = srv.RestoreSession(ctx, oapi.RestoreSessionRequestObject{Id: id})
	require.NoError(t, err)
	require.Equal(t, id, fl.restored)
	got, _ = ts.Get(ctx, id)
	require.Equal(t, terminalstore.StatusRunning, got.Status)
	require.Nil(t, got.ExitCode, "a relaunch clears the previous status")

	sig := "SIGKILL"
	srv.recordDirectTerminalEnd(id, &ptyevent.Lifecycle{Phase: ptyevent.PhaseSignaled, Signal: sig})
	got, _ = ts.Get(ctx, id)
	require.Equal(t, terminalstore.StatusOrphaned, got.Status)
	require.Nil(t, got.ExitCode)
}

// Restoring a running terminal is refused, as before.
func TestRestoreRunningTerminalConflicts(t *testing.T) {
	srv, _, _, _ := terminalGuardServer(t)
	id := spawnedTerminal(t, srv, t.TempDir())
	_, err := srv.RestoreSession(context.Background(), oapi.RestoreSessionRequestObject{Id: id})
	var ae apiError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, 409, ae.code)
}

// G7: when another trigger already relaunched the shell, restore is a
// success that converges the record — never an error, never a second process.
func TestRestoreTerminalRecordIsIdempotent(t *testing.T) {
	ctx := context.Background()
	srv, fl, _, ts := terminalGuardServer(t)
	id := spawnedTerminal(t, srv, t.TempDir())
	require.NoError(t, ts.Update(ctx, id, func(u *terminalstore.Terminal) error { u.Status = terminalstore.StatusOrphaned; return nil }))

	fl.restoreErr = lifecycle.ErrAlreadyRunning
	_, err := srv.RestoreSession(ctx, oapi.RestoreSessionRequestObject{Id: id})
	require.NoError(t, err)
	got, _ := ts.Get(ctx, id)
	require.Equal(t, terminalstore.StatusRunning, got.Status)

	// A real launch failure still surfaces and leaves the record untouched.
	require.NoError(t, ts.Update(ctx, id, func(u *terminalstore.Terminal) error { u.Status = terminalstore.StatusOrphaned; return nil }))
	fl.restoreErr = errors.New("boom")
	_, err = srv.RestoreSession(ctx, oapi.RestoreSessionRequestObject{Id: id})
	require.Error(t, err)
	got, _ = ts.Get(ctx, id)
	require.Equal(t, terminalstore.StatusOrphaned, got.Status)
}

// F-LIFE-03: hibernate then reopen keeps id, name, project and workdir; the
// shell is a labelled relaunch (fresh process), not a resumed one.
func TestHibernateReopenKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	srv, _, _, ts := terminalGuardServer(t)
	dir := t.TempDir()
	id := spawnedTerminal(t, srv, dir)
	before, _ := ts.Get(ctx, id)

	srv.hibernateProjectTerminals(ctx, before.ProjectID)
	mid, _ := ts.Get(ctx, id)
	require.Equal(t, terminalstore.StatusOrphaned, mid.Status)
	srv.restoreHibernatedTerminals(ctx, before.ProjectID)
	after, _ := ts.Get(ctx, id)

	require.Equal(t, terminalstore.StatusRunning, after.Status)
	require.Equal(t, before.ID, after.ID)
	require.Equal(t, before.Name, after.Name)
	require.Equal(t, before.ProjectID, after.ProjectID)
	require.Equal(t, before.Workdir, after.Workdir)
	require.Equal(t, before.TmuxSession, after.TmuxSession)
}

// F-LIFE-02: a spawn that cannot be recorded leaves no record and tears the
// shell down.
func TestSpawnTerminalRollbackLeavesNothing(t *testing.T) {
	ctx := context.Background()
	srv, fl, _, ts := terminalGuardServer(t)
	require.NoError(t, ts.Close()) // record write now fails

	_, err := srv.SpawnAgent(ctx, spawnTerminalReq(t.TempDir(), ""))
	require.Error(t, err)
	require.NotEmpty(t, fl.terminated, "the launched shell is terminated when the record cannot be written")
}
