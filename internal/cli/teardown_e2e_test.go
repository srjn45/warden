package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/daemon"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/mcp"
	"github.com/srjn45/warden/internal/store"
)

// gitTmuxFakeRunner runs git for real (so worktree/branch state is genuine) and
// fakes tmux: every session is dead, kill-session is a no-op.
type gitTmuxFakeRunner struct {
	// branchGone makes `git branch -D` report the branch as already deleted, as it
	// is when the branch was landed and deleted before teardown.
	branchGone bool
}

func (r gitTmuxFakeRunner) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	if name == "tmux" {
		if len(args) > 0 && args[0] == "has-session" {
			return "", exec.ErrNotFound
		}
		return "", nil
	}
	if r.branchGone && len(args) >= 4 && args[2] == "branch" && args[3] == "-D" {
		return "error: branch '" + args[4] + "' not found.", exec.ErrNotFound
	}
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	return string(out), err
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// teardownEnv is a real daemon (real agentstore + real Lifecycle over real git)
// serving a managed-worktree agent "A-1" whose branch is pushed to a bare origin.
type teardownEnv struct {
	addr, repo, wt string
	st             *agentstore.Store
}

func newTeardownEnv(t *testing.T) *teardownEnv { return newTeardownEnvWith(t, gitTmuxFakeRunner{}) }

func newTeardownEnvWith(t *testing.T, runner gitTmuxFakeRunner) *teardownEnv {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	gitOut(t, root, "init", "--bare", "-b", "main", origin)
	gitOut(t, root, "init", "-b", "main", repo)
	gitOut(t, repo, "commit", "--allow-empty", "-m", "init")
	gitOut(t, repo, "remote", "add", "origin", origin)
	gitOut(t, repo, "push", "-u", "origin", "main")
	wt := filepath.Join(repo, ".worktrees", "A-1")
	gitOut(t, repo, "worktree", "add", "-b", "A-1", wt)
	gitOut(t, wt, "push", "-u", "origin", "A-1")

	st, err := agentstore.New(filepath.Join(root, "data"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "A-1", Name: "a-1", TmuxSession: "A-1", Repo: repo, Worktree: ".worktrees/A-1",
		Branch: "A-1", BranchCreated: true, Status: store.StatusDone,
	}))

	life := daemon.NewLifecycleAdapter(lifecycle.New(runner, &lifecycle.FakeConfig{}), st)
	srv := daemon.NewServer(st, life, nil, time.Hour, false, nil, nil, nil)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.ListenAndServe(ctx, addr) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		r, err := http.Get("http://" + addr + "/api/v1/sessions")
		if err != nil {
			return false
		}
		r.Body.Close()
		return true
	}, 5*time.Second, 20*time.Millisecond)
	return &teardownEnv{addr: "http://" + addr, repo: repo, wt: wt, st: st}
}

func (e *teardownEnv) stopMCP(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ct, stt := mcpsdk.NewInMemoryTransports()
	go func() { _ = mcp.NewServer(e.addr).Run(ctx, stt) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: "stop_agent", Arguments: map[string]any{"ticket": "A-1"}})
	require.NoError(t, err)
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func (e *teardownEnv) stopCLI(t *testing.T) (string, error) {
	t.Helper()
	return runCLIStdin(t, strings.TrimPrefix(e.addr, "http://"), "", "stop", "A-1", "--yes")
}

func (e *teardownEnv) requireFullyGone(t *testing.T) {
	t.Helper()
	_, err := os.Stat(e.wt)
	require.True(t, os.IsNotExist(err), "worktree dir must be gone")
	require.Empty(t, strings.TrimSpace(gitOut(t, e.repo, "branch", "--list", "A-1")), "branch must be gone")
	_, err = e.st.Get(context.Background(), "A-1")
	require.ErrorIs(t, err, agentstore.ErrNotFound, "record must be cleared (archived)")
	closed, err := e.st.ListClosed(context.Background())
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Empty(t, closed[0].Worktree)
}

func TestStopE2E_MCPDefaultFullTeardown(t *testing.T) {
	e := newTeardownEnv(t)
	out := e.stopMCP(t)
	require.Equal(t, "stopped A-1", out)
	e.requireFullyGone(t)
}

func TestStopE2E_CLIDefaultFullTeardown(t *testing.T) {
	e := newTeardownEnv(t)
	out, err := e.stopCLI(t)
	require.NoError(t, err, out)
	require.Contains(t, out, "stopped A-1")
	e.requireFullyGone(t)
}

// A dirty worktree blocks removal, leaves the record resolvable (so the call is
// retryable), reports which steps ran, and a second call succeeds once fixed.
func TestStopE2E_DirtyBlocksThenRetrySucceeds(t *testing.T) {
	for _, via := range []string{"mcp", "cli"} {
		t.Run(via, func(t *testing.T) {
			e := newTeardownEnv(t)
			dirty := filepath.Join(e.wt, "scratch.txt")
			require.NoError(t, os.WriteFile(dirty, []byte("x"), 0o644))
			stop := func() string {
				if via == "mcp" {
					return e.stopMCP(t)
				}
				out, err := e.stopCLI(t)
				if err != nil {
					return err.Error()
				}
				return out
			}
			msg := stop()
			require.Contains(t, msg, "remove worktree failed")
			require.Contains(t, msg, "completed: terminated")
			got, err := e.st.Get(context.Background(), "A-1")
			require.NoError(t, err, "record must stay resolvable after a blocked removal")
			require.NotEmpty(t, got.Worktree)
			_, err = os.Stat(e.wt)
			require.NoError(t, err)

			require.NoError(t, os.Remove(dirty))
			stop()
			e.requireFullyGone(t)
		})
	}
}

func TestStopE2E_UnpushedBlocksThenRetrySucceeds(t *testing.T) {
	e := newTeardownEnv(t)
	gitOut(t, e.wt, "commit", "--allow-empty", "-m", "local only")
	require.Contains(t, e.stopMCP(t), "remove worktree failed")
	_, err := e.st.Get(context.Background(), "A-1")
	require.NoError(t, err)
	gitOut(t, e.wt, "push", "origin", "A-1")
	require.Equal(t, "stopped A-1", e.stopMCP(t))
	e.requireFullyGone(t)
}

// #703: the branch was already landed and deleted. Removal must succeed without
// force and clear the record's worktree pointer (no stale pointer, no 500).
func TestRemoveWorktreeE2E_PreDeletedBranchClearsRecord(t *testing.T) {
	e := newTeardownEnvWith(t, gitTmuxFakeRunner{branchGone: true})
	out, err := runCLIStdin(t, strings.TrimPrefix(e.addr, "http://"), "", "stop", "A-1", "--keep-record", "--yes")
	require.NoError(t, err, out)
	_, statErr := os.Stat(e.wt)
	require.True(t, os.IsNotExist(statErr))
	got, err := e.st.Get(context.Background(), "A-1")
	require.NoError(t, err)
	require.Empty(t, got.Worktree, "record's worktree must be cleared")
}
