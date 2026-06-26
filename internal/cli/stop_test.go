package cli

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStopDefaultFullTeardown: `wd stop <id>` confirms then terminates, clears
// the record, and removes the worktree — in that safe order.
func TestStopDefaultFullTeardown(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "y\n", "stop", "A-1")
	require.NoError(t, err)
	require.Contains(t, out, "Remove the git worktree and branch for A-1?")
	require.Contains(t, out, "stopped A-1")
	require.Equal(t, []string{
		"/api/v1/sessions/A-1/terminate",
		"/api/v1/sessions/A-1/delete",
		"/api/v1/sessions/A-1/remove-worktree",
	}, *hits)
}

// TestStopYesSkipsPrompt: --yes skips the confirmation and still does the full
// teardown.
func TestStopYesSkipsPrompt(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "", "stop", "A-1", "--yes")
	require.NoError(t, err)
	require.NotContains(t, out, "Remove the git worktree")
	require.Contains(t, out, "stopped A-1")
	require.Equal(t, []string{
		"/api/v1/sessions/A-1/terminate",
		"/api/v1/sessions/A-1/delete",
		"/api/v1/sessions/A-1/remove-worktree",
	}, *hits)
}

// TestStopConfirmDeclined: answering anything but y aborts with no daemon calls.
func TestStopConfirmDeclined(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "n\n", "stop", "A-1")
	require.NoError(t, err)
	require.Contains(t, out, "aborted")
	require.NotContains(t, out, "stopped A-1")
	require.Empty(t, *hits)
}

// TestStopKeepRecord: --keep-record terminates and removes the worktree but does
// not clear the record.
func TestStopKeepRecord(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	_, err := runCLIStdin(t, addr, "", "stop", "A-1", "--keep-record", "--yes")
	require.NoError(t, err)
	require.Equal(t, []string{
		"/api/v1/sessions/A-1/terminate",
		"/api/v1/sessions/A-1/remove-worktree",
	}, *hits)
}

// TestStopKeepWorktree: --keep-worktree == today's `done` — terminate + clear
// record, no worktree removal (and so NO confirmation prompt).
func TestStopKeepWorktree(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "", "stop", "A-1", "--keep-worktree")
	require.NoError(t, err)
	require.NotContains(t, out, "Remove the git worktree")
	require.Equal(t, []string{
		"/api/v1/sessions/A-1/terminate",
		"/api/v1/sessions/A-1/delete",
	}, *hits)
}

// TestStopKeepBoth: --keep-record --keep-worktree == terminate only.
func TestStopKeepBoth(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	_, err := runCLIStdin(t, addr, "", "stop", "A-1", "--keep-record", "--keep-worktree")
	require.NoError(t, err)
	require.Equal(t, []string{"/api/v1/sessions/A-1/terminate"}, *hits)
}

// TestStopPRFirst: --pr opens the PR before any teardown step (safe order).
func TestStopPRFirst(t *testing.T) {
	addr, hits := doneStub(t, map[string]any{"url": "https://github.com/o/r/pull/7", "created": true}, 0)
	out, err := runCLIStdin(t, addr, "", "stop", "A-1", "--pr", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "opened PR: https://github.com/o/r/pull/7")
	require.Contains(t, out, "stopped A-1")
	require.Equal(t, []string{
		"/api/v1/sessions/A-1/create-pr",
		"/api/v1/sessions/A-1/terminate",
		"/api/v1/sessions/A-1/delete",
		"/api/v1/sessions/A-1/remove-worktree",
	}, *hits)
}

// TestStopPRFailureLeavesAgentRunning: a failed PR aborts before terminate.
func TestStopPRFailureLeavesAgentRunning(t *testing.T) {
	addr, hits := doneStub(t, nil, http.StatusConflict)
	_, err := runCLIStdin(t, addr, "", "stop", "A-1", "--pr", "--yes")
	require.Error(t, err)
	require.Contains(t, err.Error(), "create PR")
	require.Equal(t, []string{"/api/v1/sessions/A-1/create-pr"}, *hits)
}

// --- alias behavior must NOT change ---

// TestTerminateAlias: terminate hits only /terminate and keeps record+worktree.
func TestTerminateAlias(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "", "terminate", "A-1")
	require.NoError(t, err)
	require.Contains(t, out, "terminated A-1")
	require.Equal(t, []string{"/api/v1/sessions/A-1/terminate"}, *hits)
}

// TestDeleteAlias: delete hits only /delete.
func TestDeleteAlias(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "", "delete", "A-1")
	require.NoError(t, err)
	require.Contains(t, out, "deleted A-1")
	require.Equal(t, []string{"/api/v1/sessions/A-1/delete"}, *hits)
}

// TestRemoveWorktreeAliasPrompts: remove-worktree always asks; declining aborts.
func TestRemoveWorktreeAliasPrompts(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "n\n", "remove-worktree", "A-1")
	require.NoError(t, err)
	require.Contains(t, out, "Remove the git worktree and branch for A-1?")
	require.Contains(t, out, "aborted")
	require.Empty(t, *hits)
}

// TestRemoveWorktreeAliasYes: --yes skips the prompt and removes the worktree.
func TestRemoveWorktreeAliasYes(t *testing.T) {
	addr, hits := doneStub(t, nil, 0)
	out, err := runCLIStdin(t, addr, "", "remove-worktree", "A-1", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "removed worktree for A-1")
	require.Equal(t, []string{"/api/v1/sessions/A-1/remove-worktree"}, *hits)
}
