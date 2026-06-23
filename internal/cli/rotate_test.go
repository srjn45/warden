package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

func TestBuildSuccessorParams(t *testing.T) {
	old := &store.Session{Workdir: "/repo/.worktrees/CRD-1", PermissionMode: "acceptEdits", Repo: "/repo", Worktree: "/repo/.worktrees/CRD-1"}
	p := buildSuccessorParams(old, "do the thing")
	require.Equal(t, "do the thing", p.Prompt)
	require.Equal(t, "/repo/.worktrees/CRD-1", p.Cwd, "successor must launch in the old agent's workdir (the worktree)")
	require.Equal(t, "acceptEdits", p.PermissionMode, "successor inherits permission mode")
	// Prompt-mode spawn: no Type/Repo/Worktree, so the existing worktree is reused by cwd, not recreated.
	require.Empty(t, p.Type)
	require.Empty(t, p.Repo)
	require.False(t, p.Worktree)
}

func TestComposeSuccessorPrompt(t *testing.T) {
	handoff := "/tmp/warden-rotate-handoff-agent-abc123.md"
	got := composeSuccessorPrompt("Finish the migration.", handoff)
	require.Contains(t, got, handoff, "must point successor at the handoff file")
	require.Contains(t, got, "Finish the migration.", "must include the human-reviewed resume prompt")
	require.Contains(t, got, "delete", "must tell the successor to clean up the temp handoff file once read")
}

func TestSelfSessionID(t *testing.T) {
	t.Setenv("AGENTCTL_SESSION_ID", "")
	t.Setenv("WARDEN_SESSION_ID", "agent-abc123")
	id, err := selfSessionID()
	require.NoError(t, err)
	require.Equal(t, "agent-abc123", id)

	// Legacy AGENTCTL_SESSION_ID still resolves when WARDEN_ is unset.
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "agent-legacy")
	id, err = selfSessionID()
	require.NoError(t, err)
	require.Equal(t, "agent-legacy", id)

	t.Setenv("AGENTCTL_SESSION_ID", "")
	_, err = selfSessionID()
	require.Error(t, err, "must error when not run inside an agent session")
}

func TestValidateHandoff(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "nope.md")
	require.Error(t, validateHandoff(missing), "missing file is an error")

	empty := filepath.Join(dir, "empty.md")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	require.Error(t, validateHandoff(empty), "empty file is an error")

	good := filepath.Join(dir, "good.md")
	require.NoError(t, os.WriteFile(good, []byte("notes"), 0o644))
	require.NoError(t, validateHandoff(good))

	aDir := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(aDir, 0o755))
	require.Error(t, validateHandoff(aDir), "a directory is not a valid handoff file")
}

type fakeRotator struct {
	getSession   *store.Session
	getErr       error
	spawnResult  *store.Session
	spawnErr     error
	terminateErr error
	calls        []string
	spawnParams  client.SpawnParams
}

func (f *fakeRotator) Get(ctx context.Context, id string) (*store.Session, error) {
	f.calls = append(f.calls, "get:"+id)
	return f.getSession, f.getErr
}
func (f *fakeRotator) Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error) {
	f.calls = append(f.calls, "spawn")
	f.spawnParams = p
	return f.spawnResult, f.spawnErr
}
func (f *fakeRotator) Terminate(ctx context.Context, id string) error {
	f.calls = append(f.calls, "terminate:"+id)
	return f.terminateErr
}

func TestRunRotateHappyPath(t *testing.T) {
	f := &fakeRotator{
		getSession:  &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x", PermissionMode: "acceptEdits"},
		spawnResult: &store.Session{ID: "agent-new", Workdir: "/repo/.worktrees/x"},
	}
	// onSpawned closes over f so its call interleaves into the same ordered log.
	succ, err := runRotate(context.Background(), f, "agent-old", "resume prompt",
		func(s *store.Session) { f.calls = append(f.calls, "print:"+s.ID) })
	require.NoError(t, err)
	require.Equal(t, "agent-new", succ.ID)
	require.Equal(t,
		[]string{"get:agent-old", "spawn", "print:agent-new", "terminate:agent-old"},
		f.calls,
		"summary must print AFTER spawn but BEFORE reap — the reap kills this very process in self-rotation")
	require.Equal(t, "/repo/.worktrees/x", f.spawnParams.Cwd)
	require.Equal(t, "acceptEdits", f.spawnParams.PermissionMode)
}

func TestRunRotateSpawnErrorDoesNotReap(t *testing.T) {
	f := &fakeRotator{
		getSession: &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x"},
		spawnErr:   errors.New("spawn boom"),
	}
	printed := false
	succ, err := runRotate(context.Background(), f, "agent-old", "resume prompt",
		func(s *store.Session) { printed = true })
	require.Error(t, err)
	require.Nil(t, succ, "no successor on spawn failure")
	require.False(t, printed, "must not print a success summary when spawn fails")
	require.Equal(t, []string{"get:agent-old", "spawn"}, f.calls, "must NOT terminate the old agent when spawn fails")
}

func TestRunRotateReapErrorStillReturnsSuccessor(t *testing.T) {
	f := &fakeRotator{
		getSession:   &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x"},
		spawnResult:  &store.Session{ID: "agent-new"},
		terminateErr: errors.New("terminate boom"),
	}
	succ, err := runRotate(context.Background(), f, "agent-old", "resume prompt", func(s *store.Session) {})
	require.Error(t, err, "reap failure is surfaced")
	require.NotNil(t, succ, "successor is already live, so it is returned")
	require.Equal(t, "agent-new", succ.ID)
}

func TestRunRotateToleratesNilCallback(t *testing.T) {
	f := &fakeRotator{
		getSession:  &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x"},
		spawnResult: &store.Session{ID: "agent-new"},
	}
	_, err := runRotate(context.Background(), f, "agent-old", "resume prompt", nil)
	require.NoError(t, err)
}
