package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// Recovery attempts on a degraded store never touch running agents: tmux
// sessions (and their pane processes) survive daemon start, doctor, repair and
// membership reconcile unchanged, and the client library every TUI/CLI view is
// built on agrees with the HTTP surfaces.
func TestRecoveryWhileTmuxSessionsExist(t *testing.T) {
	e := newEnv(t)
	e.seed("a-1", "a-2", "a-3")
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		require.NoError(t, e.tmux("new-session", "-d", "-s", "tm-"+id, "sleep", "600"))
	}
	// plus an unknown (unmanaged) live session that recovery must not adopt/kill.
	require.NoError(t, e.tmux("new-session", "-d", "-s", "stray", "sleep", "600"))
	panes := e.tmuxPanePIDs()
	require.Contains(t, panes, "tm-a-1:")

	faults["in-range-wrong-record"](e)
	before := agentsDB(t, e)

	d := e.startDaemon(freeAddr(t), e.data, e.root)
	d.waitUp()

	// lookup/list/tree/health/client agree: degraded, fail closed.
	c := client.New(d.base())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, lerr := c.List(ctx)
	require.Error(t, lerr, "client list must fail closed on a degraded store")
	h, herr := c.StoreHealth(ctx)
	require.NoError(t, herr)
	require.False(t, h.Healthy)
	require.True(t, h.RepairAvailable)
	for _, p := range []string{"/api/v1/sessions", "/api/v1/tree"} {
		code, _ := get(t, d.base()+p)
		require.Equal(t, 503, code, p)
	}
	require.False(t, d.health().Healthy)

	cfg := e.config(d.addr, e.data)
	out, _ := e.run(e.root, "--config", cfg, "doctor")
	require.Contains(t, out, "DEGRADED", out)
	d.stop()

	cfg = e.config(freeAddr(t), e.data)
	out, code := e.run(e.root, "--config", cfg, "repair", "agents", "--dry-run")
	require.Zero(t, code, out)
	require.Contains(t, out, "no files changed", out)
	require.NotContains(t, out, "0 finding(s)", "dry-run must report the injected damage")
	_, code = e.run(e.root, "--config", cfg, "doctor", "--reconcile-membership")
	_ = code // no projects configured: nothing to reconcile either way

	requireSame(t, before, agentsDB(t, e), "recovery attempts mutated agents-db")
	require.Equal(t, panes, e.tmuxPanePIDs(), "recovery attempts disturbed live tmux sessions")
}

// Stale membership and duplicate identities: reconcile reports conflicts and
// leaves conflicted/unverified agents untouched; ghosts are tolerated, never
// pruned; only the verified agent is stamped.
func TestMembershipConflictsAndStaleEntries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, err := agentstore.New(e.data)
	require.NoError(t, err)
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		require.NoError(t, s.Insert(ctx, &agentstore.Agent{ID: id, Name: id, Status: store.StatusWorking}))
	}
	// a-1: duplicate identity (archived copy AND a fresh active record).
	require.NoError(t, s.Archive(ctx, "a-1"))
	require.NoError(t, s.Insert(ctx, &agentstore.Agent{ID: "a-1", Name: "a-1", Status: store.StatusWorking}))
	require.NoError(t, s.Close())

	ps, err := projectstore.NewStore(filepath.Join(e.data, "projects"))
	require.NoError(t, err)
	repoA, repoB := filepath.Join(e.root, "repoA"), filepath.Join(e.root, "repoB")
	// ghost = stale member (no such agent); a-2 claimed by BOTH projects (ambiguous); a-3 clean.
	require.NoError(t, ps.Upsert(projectstore.Project{ID: repoA, Name: "A", Path: repoA, Agents: []string{"a-1", "a-2", "a-3", "ghost"}}))
	require.NoError(t, ps.Upsert(projectstore.Project{ID: repoB, Name: "B", Path: repoB, Agents: []string{"a-2"}}))
	require.NoError(t, ps.Close())

	cfg := e.config(freeAddr(t), e.data)
	d := e.startDaemon(freeAddr(t), e.data, e.root)
	d.waitUp()
	// reconcile refuses while the daemon owns the stores.
	out, code := e.run(e.root, "--config", cfg, "doctor", "--reconcile-membership")
	require.NotZero(t, code, out)
	d.stop()

	out, code = e.run(e.root, "--config", cfg, "doctor", "--reconcile-membership")
	require.Zero(t, code, out)
	require.Contains(t, out, "CONFLICT", out)
	for _, id := range []string{"a-1", "a-2"} {
		require.Contains(t, out, id, out)
	}
	require.False(t, strings.Contains(out, "CONFLICT agent a-3"), out)

	// idempotent: a second run changes nothing.
	again, code := e.run(e.root, "--config", cfg, "doctor", "--reconcile-membership")
	require.Zero(t, code, again)
	require.Contains(t, again, "sessions stamped:  0", again)

	// conflicted agents unstamped; ghost still a member of record.
	s, err = agentstore.New(e.data)
	require.NoError(t, err)
	for _, id := range []string{"a-1", "a-2"} {
		a, err := s.Get(ctx, id)
		require.NoError(t, err)
		require.Empty(t, a.ProjectID, "%s is conflicted and must stay unstamped", id)
	}
	require.NoError(t, s.Close())
	ps, err = projectstore.NewStore(filepath.Join(e.data, "projects"))
	require.NoError(t, err)
	defer ps.Close()
	pa, err := ps.Get(repoA)
	require.NoError(t, err)
	require.Contains(t, pa.Agents, "ghost", "stale members are tolerated, never pruned")
}
