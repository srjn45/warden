package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// freeAddr reserves an ephemeral port and returns its address. There is a tiny
// race between closing the listener and re-binding, but it is harmless in tests.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func TestListenAndServeShutsDownGracefully(t *testing.T) {
	srv := NewServer(newFakeStore(), &fakeLife{}, nil, time.Second, false, nil, nil, nil)
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx, addr) }()

	// Wait until the server is actually accepting requests.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)

	cancel() // trigger graceful shutdown
	select {
	case err := <-errCh:
		require.NoError(t, err, "graceful shutdown should return without error")
	case <-time.After(shutdownGrace + 2*time.Second):
		t.Fatal("ListenAndServe did not return after ctx cancel")
	}
}

func TestStartupDoesNotRunConflictDetectorGitPolling(t *testing.T) {
	// Branch tracking is separately opt-in and may intentionally invoke Git.
	// With its default disabled interval, daemon startup must not start any
	// detector-era Git polling of agent worktrees.
	worktree := t.TempDir()
	fs := newFakeStore()
	fs.data["active-agent"] = &agentstore.Agent{
		ID:          "active-agent",
		Status:      store.StatusWorking,
		TmuxSession: "active-agent",
		Worktree:    worktree,
	}

	binDir := t.TempDir()
	marker := filepath.Join(binDir, "git-called")
	git := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(git, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0o755))
	t.Setenv("PATH", binDir)

	srv := NewServer(fs, &fakeLife{}, nil, time.Second, false, nil, nil, nil)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx, addr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(shutdownGrace + 2*time.Second):
			t.Error("ListenAndServe did not return after ctx cancel")
		}
	})

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	}, 2*time.Second, 10*time.Millisecond)
	require.Never(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, 300*time.Millisecond, 10*time.Millisecond,
		"startup must not invoke detector Git polling for an active worktree")
}
