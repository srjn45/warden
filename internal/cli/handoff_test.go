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

func TestReadHandoffContent(t *testing.T) {
	dir := t.TempDir()

	_, err := readHandoffContent(filepath.Join(dir, "nope.md"))
	require.Error(t, err, "missing file is an error")

	empty := filepath.Join(dir, "empty.md")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	_, err = readHandoffContent(empty)
	require.Error(t, err, "empty file is an error")

	good := filepath.Join(dir, "good.md")
	require.NoError(t, os.WriteFile(good, []byte("the notes"), 0o644))
	body, err := readHandoffContent(good)
	require.NoError(t, err)
	require.Equal(t, "the notes", body, "must return the file body for inlining")
}

func TestComposeDelegatePrompt(t *testing.T) {
	got := composeDelegatePrompt("Implement the parser.", "branch: feat-x\ngoal: parse it")
	require.Contains(t, got, "Implement the parser.", "must include the task prompt")
	require.Contains(t, got, "goal: parse it", "must inline the handoff content (not a path)")
	require.Contains(t, got, "delegated", "must frame the agent as receiving a delegated task")
}

func TestComposeHandoffMessage(t *testing.T) {
	got := composeHandoffMessage("Take over the migration.", "decisions: use sqlc", "agent-src")
	require.Contains(t, got, "agent-src", "must name the sender for provenance")
	require.Contains(t, got, "decisions: use sqlc", "must inline the handoff content")
	require.Contains(t, got, "Take over the migration.", "must include the ask")
}

func TestBuildDelegateParams(t *testing.T) {
	p := buildDelegateParams("/repo", "development", "delegate-1", "feat-x", "do it", false)
	require.Equal(t, "development", p.Type)
	require.Equal(t, "/repo", p.Repo)
	require.Equal(t, "delegate-1", p.Name)
	require.Equal(t, "feat-x", p.Branch)
	require.Equal(t, "do it", p.Prompt)
	// Managed spawn: Worktree/InRepo left false so the write-agent isolation
	// default gives the delegate its own worktree — never the source's.
	require.False(t, p.Worktree)
	require.False(t, p.InRepo)
	require.Empty(t, p.Cwd, "a delegate must not inherit the source's cwd")
	require.False(t, p.Force, "force defaults off — the gate is respected unless asked")

	forced := buildDelegateParams("/repo", "development", "", "", "do it", true)
	require.True(t, forced.Force, "--force threads through to the spawn past the memory-pressure gate")
}

func TestResolveHandoffRepo(t *testing.T) {
	got, err := resolveHandoffRepo("/flag/repo", &store.Session{Repo: "/session/repo"})
	require.NoError(t, err)
	require.Equal(t, "/flag/repo", got, "--repo flag wins")

	got, err = resolveHandoffRepo("", &store.Session{Repo: "/session/repo"})
	require.NoError(t, err)
	require.Equal(t, "/session/repo", got, "falls back to the source session's repo")

	got, err = resolveHandoffRepo("", nil)
	require.NoError(t, err)
	cwd, _ := os.Getwd()
	require.Equal(t, cwd, got, "falls back to cwd when no flag and no source session")
}

type fakeHandoffClient struct {
	getSession  *store.Session
	getErr      error
	spawnResult *store.Session
	spawnErr    error
	msgWoke     bool
	msgErr      error
	calls       []string
	spawnParams client.SpawnParams
	msgTo       string
	msgFrom     string
	msgBody     string
}

func (f *fakeHandoffClient) Get(ctx context.Context, id string) (*store.Session, error) {
	f.calls = append(f.calls, "get:"+id)
	return f.getSession, f.getErr
}
func (f *fakeHandoffClient) Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error) {
	f.calls = append(f.calls, "spawn")
	f.spawnParams = p
	return f.spawnResult, f.spawnErr
}
func (f *fakeHandoffClient) MsgSend(ctx context.Context, to, from, body string) (client.Message, bool, error) {
	f.calls = append(f.calls, "msgsend:"+to)
	f.msgTo, f.msgFrom, f.msgBody = to, from, body
	return client.Message{ID: "m1"}, f.msgWoke, f.msgErr
}

func TestRunHandoffNewHappyPath(t *testing.T) {
	f := &fakeHandoffClient{spawnResult: &store.Session{ID: "agent-new", Type: "development"}}
	params := buildDelegateParams("/repo", "development", "", "", "prompt", false)
	delegate, err := runHandoffNew(context.Background(), f, params)
	require.NoError(t, err)
	require.Equal(t, "agent-new", delegate.ID)
	require.Equal(t, []string{"spawn"}, f.calls, "new mode spawns and never terminates the source")
	require.Equal(t, "/repo", f.spawnParams.Repo)
}

func TestRunHandoffNewSpawnError(t *testing.T) {
	f := &fakeHandoffClient{spawnErr: errors.New("boom")}
	delegate, err := runHandoffNew(context.Background(), f, client.SpawnParams{})
	require.Error(t, err)
	require.Nil(t, delegate)
}

func TestRunHandoffToHappyPath(t *testing.T) {
	f := &fakeHandoffClient{getSession: &store.Session{ID: "agent-tgt"}, msgWoke: true}
	woke, err := runHandoffTo(context.Background(), f, "agent-tgt", "agent-src", "the body")
	require.NoError(t, err)
	require.True(t, woke)
	require.Equal(t, []string{"get:agent-tgt", "msgsend:agent-tgt"}, f.calls,
		"must verify the target exists BEFORE sending")
	require.Equal(t, "agent-src", f.msgFrom)
	require.Equal(t, "the body", f.msgBody)
}

func TestRunHandoffToMissingTarget(t *testing.T) {
	f := &fakeHandoffClient{getErr: errors.New("not found")}
	_, err := runHandoffTo(context.Background(), f, "ghost", "agent-src", "body")
	require.Error(t, err)
	require.Equal(t, []string{"get:ghost"}, f.calls, "must NOT send when the target does not exist")
}
