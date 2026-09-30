package daemon

import (
	"context"
	"github.com/srjn45/warden/internal/agentstore"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/stretchr/testify/require"
)

// TestTerminalLifecycleThroughTerminalstore covers the ownership cutover:
// spawn/list/terminate/hibernate/restore route through terminalstore, and a
// re-opened terminalstore (daemon restart) still resolves the record.
func TestTerminalLifecycleThroughTerminalstore(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	workdir := t.TempDir()

	ts, err := terminalstore.New(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })

	fs := newFakeStore()
	fl := &fakeLife{}
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	proj, err := ps.OpenProject(workdir, "proj", workdir)
	require.NoError(t, err)

	srv := &Server{store: fs, life: fl, projects: ps, terminals: ts, hub: newHub(), done: make(chan struct{})}

	// Spawn a terminal — must land in terminalstore, not the agent session store.
	resp, err := srv.SpawnAgent(ctx, oapi.SpawnAgentRequestObject{
		Body: &oapi.SpawnRequest{Cwd: workdir, Kind: oapi.SpawnRequestKindTerminal, ProjectId: proj.ID},
	})
	require.NoError(t, err)
	created, ok := resp.(oapi.SpawnAgent201JSONResponse)
	require.True(t, ok)
	require.Equal(t, store.KindTerminal, created.Kind)
	termID := created.ID

	_, err = fs.Get(ctx, termID)
	require.ErrorIs(t, err, agentstore.ErrNotFound, "terminal must not be inserted into the agent session store")

	got, err := ts.Get(ctx, termID)
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusRunning, got.Status)
	require.Equal(t, proj.ID, got.ProjectID)

	// List/discovery surfaces the terminal via terminalstore projection.
	list, err := srv.ListSessions(ctx, oapi.ListSessionsRequestObject{
		Params: oapi.ListSessionsParams{Kind: oapi.ListSessionsParamsKindTerminal},
	})
	require.NoError(t, err)
	listed := list.(oapi.ListSessions200JSONResponse)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, termID, listed.Sessions[0].ID)

	get, err := srv.GetSession(ctx, oapi.GetSessionRequestObject{Id: termID})
	require.NoError(t, err)
	_, ok = get.(oapi.GetSession200JSONResponse)
	require.True(t, ok)

	// Project hibernation: kill pane, keep orphaned record.
	srv.hibernateProjectTerminals(ctx, proj.ID)
	require.Equal(t, created.TmuxSession, fl.terminated)
	hib, err := ts.Get(ctx, termID)
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusOrphaned, hib.Status)

	// Reopen restores the shell via RestoreTerminal.
	fl.terminated = ""
	fl.restored = ""
	srv.restoreHibernatedTerminals(ctx, proj.ID)
	require.Equal(t, termID, fl.restored)
	run, err := ts.Get(ctx, termID)
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusRunning, run.Status)

	// Daemon restart survival: reopen terminalstore on the same data dir.
	require.NoError(t, ts.Close())
	ts2, err := terminalstore.New(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts2.Close() })
	survived, err := ts2.Get(ctx, termID)
	require.NoError(t, err)
	require.Equal(t, termID, survived.ID)
	require.Equal(t, terminalstore.StatusRunning, survived.Status)

	// Operator terminate deletes the durable terminal record.
	srv.terminals = ts2
	_, err = srv.TerminateSession(ctx, oapi.TerminateSessionRequestObject{Id: termID})
	require.NoError(t, err)
	_, err = ts2.Get(ctx, termID)
	require.ErrorIs(t, err, terminalstore.ErrNotFound)
}

func TestTerminalPollerDepsUpdatesTerminalstore(t *testing.T) {
	ctx := context.Background()
	ts, err := terminalstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })

	now := time.Now().UTC()
	require.NoError(t, ts.Insert(ctx, &terminalstore.Terminal{
		ID: "term-poll", ProjectID: "p1", TmuxSession: "term-poll",
		Status: terminalstore.StatusRunning, CreatedAt: now, UpdatedAt: now,
	}))

	host := &fakeTmuxHost{alive: map[string]bool{"term-poll": false}}
	deps := NewTerminalPollerDeps(ts, host, nil)
	w := NewTerminalWatcherForTest(deps)
	w.tick(ctx)

	got, err := ts.Get(ctx, "term-poll")
	require.NoError(t, err)
	require.Equal(t, terminalstore.StatusOrphaned, got.Status)
}

// NewTerminalWatcherForTest exposes tick for package-local tests via poller.
// Defined here so daemon tests can drive one tick without exporting poller.tick.
func NewTerminalWatcherForTest(deps interface {
	List(context.Context) ([]*store.Session, error)
	SessionAlive(context.Context, string) bool
	CapturePane(context.Context, string) (string, error)
	UpdateStatusIf(context.Context, string, store.Status, store.Status) (bool, error)
	UpdatePane(context.Context, string, string) error
	ExitCode(context.Context, string) (int, bool)
	ClearExit(context.Context, string)
	Restore(context.Context, *store.Session) error
}) *terminalTickDriver {
	return &terminalTickDriver{deps: deps}
}

type terminalTickDriver struct {
	deps interface {
		List(context.Context) ([]*store.Session, error)
		SessionAlive(context.Context, string) bool
		CapturePane(context.Context, string) (string, error)
		UpdateStatusIf(context.Context, string, store.Status, store.Status) (bool, error)
		UpdatePane(context.Context, string, string) error
		ExitCode(context.Context, string) (int, bool)
		ClearExit(context.Context, string)
		Restore(context.Context, *store.Session) error
	}
}

func (d *terminalTickDriver) tick(ctx context.Context) {
	// Inline the orphan path the TerminalWatcher uses so we don't import
	// poller's unexported tick from another package.
	sessions, err := d.deps.List(ctx)
	if err != nil {
		return
	}
	for _, s := range sessions {
		if !s.IsTerminal() {
			continue
		}
		if d.deps.SessionAlive(ctx, s.TmuxSession) {
			continue
		}
		if s.Status == store.StatusOrphaned {
			continue
		}
		_, _ = d.deps.UpdateStatusIf(ctx, s.ID, s.Status, store.StatusOrphaned)
	}
}

type fakeTmuxHost struct {
	alive map[string]bool
}

func (f *fakeTmuxHost) NewSession(context.Context, string, string, ...string) error { return nil }
func (f *fakeTmuxHost) KillSession(context.Context, string) error                   { return nil }
func (f *fakeTmuxHost) HasSession(_ context.Context, name string) bool              { return f.alive[name] }
func (f *fakeTmuxHost) CapturePane(context.Context, string) (string, error)         { return "", nil }
func (f *fakeTmuxHost) SendKeys(context.Context, string, ...string) error           { return nil }
