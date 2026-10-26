package e2e

import (
	"context"
	"os/exec"
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

// Recovery attempts never touch running agents: tmux sessions (and their pane
// processes) survive daemon start, doctor, repair and membership reconcile
// unchanged — including when the on-disk index lies and when the daemon must
// refuse a corrupt segment — and the client library every TUI/CLI view is
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
	before := segments(t, e)

	d := e.startDaemon(freeAddr(t), e.data, e.root)
	d.waitUp()

	// the healed fleet is identical through the client library and HTTP.
	c := client.New(d.base())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listed, lerr := c.List(ctx)
	require.NoError(t, lerr)
	require.Len(t, listed, 3)
	h, herr := c.StoreHealth(ctx)
	require.NoError(t, herr)
	require.True(t, h.Healthy)
	code, got := fleet(t, d)
	require.Equal(t, 200, code)
	require.Equal(t, []string{"tm-a-1", "tm-a-2", "tm-a-3"}, got)

	cfg := e.config(d.addr, e.data)
	out, _ := e.run(e.root, "--config", cfg, "doctor")
	require.NotContains(t, out, "DEGRADED", out)
	d.stop()

	cfg = e.config(freeAddr(t), e.data)
	out, code = e.run(e.root, "--config", cfg, "repair", "warden-db", "--dry-run")
	require.Zero(t, code, out)
	_, code = e.run(e.root, "--config", cfg, "doctor", "--reconcile-membership")
	_ = code // no projects configured: nothing to reconcile either way

	requireSame(t, before, segments(t, e), "recovery attempts mutated record segments")
	require.Equal(t, panes, e.tmuxPanePIDs(), "recovery attempts disturbed live tmux sessions")
}

// Stale membership and duplicate identities: reconcile reports conflicts and
// leaves conflicted/unverified agents untouched; ghosts are tolerated, never
// pruned; only the verified agent is stamped.
func TestMembershipConflictsAndStaleEntries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s, closeDB := e.openSharedSchema2()
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		require.NoError(t, s.Insert(ctx, &agentstore.Agent{ID: id, Name: id, Scope: "system", Status: store.StatusWorking}))
	}
	// a-1: duplicate identity (archived copy AND a fresh active record).
	require.NoError(t, s.Archive(ctx, "a-1"))
	require.NoError(t, s.Insert(ctx, &agentstore.Agent{ID: "a-1", Name: "a-1", Scope: "system", Status: store.StatusWorking}))
	closeDB()

	ps, closePS := e.openSharedProjectsSchema2()
	repoA, repoB := filepath.Join(e.root, "repoA"), filepath.Join(e.root, "repoB")
	// ghost = stale member (no such agent); a-2 claimed by BOTH projects (ambiguous); a-3 clean.
	require.NoError(t, ps.Upsert(projectstore.Project{ID: repoA, Name: "A", Path: repoA, Agents: []string{"a-1", "a-2", "a-3", "ghost"}}))
	require.NoError(t, ps.Upsert(projectstore.Project{ID: repoB, Name: "B", Path: repoB, Agents: []string{"a-2"}}))
	closePS()

	cfg := e.config(freeAddr(t), e.data)
	// Historical schema-2 generation: the released waypoint owns it.
	waypoint := waypointBinary(t)
	run := func(args ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		out, err := e.cmdBin(ctx, waypoint, e.root, args...).CombinedOutput()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			require.True(t, ok, "run %v: %v", args, err)
			code = ee.ExitCode()
		}
		return string(out), code
	}
	d := e.startDaemonBin(waypoint, freeAddr(t), e.data, e.root)
	d.waitUp()
	// reconcile refuses while the daemon owns the stores.
	out, code := run("--config", cfg, "doctor", "--reconcile-membership")
	require.NotZero(t, code, out)
	d.stop()

	out, code = run("--config", cfg, "doctor", "--reconcile-membership")
	require.Zero(t, code, out)
	require.Contains(t, out, "CONFLICT", out)
	for _, id := range []string{"a-1", "a-2"} {
		require.Contains(t, out, id, out)
	}
	require.False(t, strings.Contains(out, "CONFLICT agent a-3"), out)

	// idempotent: a second run changes nothing.
	again, code := run("--config", cfg, "doctor", "--reconcile-membership")
	require.Zero(t, code, again)
	require.Contains(t, again, "sessions stamped:  0", again)

	// conflicted agents unstamped; ghost still a member of record.
	s, closeDB = e.openSharedSchema2()
	for _, id := range []string{"a-1", "a-2"} {
		a, err := s.Get(ctx, id)
		require.NoError(t, err)
		require.Empty(t, a.ProjectID, "%s is conflicted and must stay unstamped", id)
	}
	closeDB()
	ps, closePS = e.openSharedProjectsSchema2()
	defer closePS()
	pa, err := ps.Get(repoA)
	require.NoError(t, err)
	require.Contains(t, pa.Agents, "ghost", "stale members are tolerated, never pruned")
}
