//go:build unix

package lifecycle

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Canceling Run must kill the whole process group, including a grandchild that
// holds stdout open, and return promptly instead of waiting for it.
func TestExecRunnerCancelKillsProcessGroup(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, "", "sh", "-c", "sleep 30 & echo $! > "+pidFile+"; wait")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)

	raw, rerr := os.ReadFile(pidFile)
	require.NoError(t, rerr)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, perr)
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil // grandchild gone
	}, 3*time.Second, 20*time.Millisecond)
}

// A subprocess that ignores SIGTERM (slow/stuck provider cleanup) and spawns a
// grandchild must still be gone shortly after the caller's deadline.
func TestExecRunnerCancelKillsTermIgnoringGroup(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, "", "sh", "-c",
		"trap '' TERM; (trap '' TERM; sleep 30) & echo $! > "+pidFile+"; while :; do sleep 1; done")
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)

	raw, rerr := os.ReadFile(pidFile)
	require.NoError(t, rerr)
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, perr)
	require.Eventually(t, func() bool { return syscall.Kill(pid, 0) != nil }, 3*time.Second, 20*time.Millisecond)
}
