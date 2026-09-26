package daemon

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/srjn45/warden/internal/lifecycle"
	wardenmcp "github.com/srjn45/warden/internal/mcp"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// Exercise the actual MCP transport, HTTP routing, adapter, and Git runner. A
// fake Commit result cannot detect a commit accidentally made in the checkout.
func TestMCPCommitExplicitLinkedWorktreeE2E(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, wt := setupRepoWithWorktree(t)
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	originalHead := git(repo, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "original.txt"), []byte("leave dirty\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "linked.txt"), []byte("commit me\n"), 0o644))
	originalStatus := git(repo, "status", "--porcelain")
	fs := newFakeStore()
	require.NoError(t, fs.Insert(context.Background(), &store.Session{ID: "e2e-agent", Repo: repo, Workdir: repo, Status: store.StatusWorking}))
	lc := lifecycle.New(lifecycle.ExecRunner{}, &lifecycle.FakeConfig{})
	srv := &Server{store: fs, life: NewLifecycleAdapter(lc, fs)}
	daemon := httptest.NewServer(srv.router())
	defer daemon.Close()
	t.Setenv("WARDEN_SESSION_ID", "e2e-agent")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = wardenmcp.NewServer(daemon.URL).Run(ctx, serverTransport) }()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "commit", Arguments: map[string]any{"dir": wt, "message": "linked worktree regression"}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%+v", result.Content)
	require.NotEqual(t, originalHead, git(wt, "rev-parse", "HEAD"))
	require.Equal(t, "linked worktree regression", git(wt, "log", "-1", "--format=%s"))
	require.Equal(t, "linked.txt", git(wt, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
	require.Empty(t, git(wt, "status", "--porcelain"))
	require.Equal(t, originalHead, git(repo, "rev-parse", "HEAD"))
	require.Equal(t, originalStatus, git(repo, "status", "--porcelain"))

	unrelated, _ := setupRepoWithWorktree(t)
	unrelatedHead := git(unrelated, "rev-parse", "HEAD")
	result, err = session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "commit", Arguments: map[string]any{"dir": unrelated, "message": "must fail"}})
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	errorText, ok := result.Content[0].(*mcpsdk.TextContent)
	require.True(t, ok)
	require.Contains(t, errorText.Text, "outside the agent's repository/worktree")
	require.Equal(t, unrelatedHead, git(unrelated, "rev-parse", "HEAD"))
	require.Equal(t, originalHead, git(repo, "rev-parse", "HEAD"))
	require.Equal(t, originalStatus, git(repo, "status", "--porcelain"))
}
