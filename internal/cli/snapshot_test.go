package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/snapshot"
)

func TestSnapshotTargetPrefersExplicitNameThenEnv(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "self-agent")
	t.Setenv("AGENTCTL_SESSION_ID", "")

	session, dir := snapshotTarget("other-agent")
	require.Equal(t, "other-agent", session, "an explicit name wins")
	require.NotEmpty(t, dir, "dir is always the cwd")

	session, _ = snapshotTarget("")
	require.Equal(t, "self-agent", session, "no name falls back to WARDEN_SESSION_ID")
}

func TestFirstArg(t *testing.T) {
	require.Equal(t, "", firstArg(nil))
	require.Equal(t, "x", firstArg([]string{"x"}))
	require.Equal(t, "x", firstArg([]string{"x", "y"}))
}

func TestShortSHA(t *testing.T) {
	require.Equal(t, "abc1234", shortSHA("abc1234"))        // short SHA unchanged
	require.Equal(t, "abcdef12", shortSHA("abcdef1234567")) // truncated to 8
	require.Equal(t, "", shortSHA(""))
}

func TestPrintSnapshotRestoreCleanApply(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := printSnapshotRestore(cmd, &snapshot.RestoreResult{
		SnapshotID: "snap-1", Branch: "feature-x", Applied: true, HeadMatch: true,
		TranscriptPath: "/data/snap-1.transcript",
	})
	require.NoError(t, err)
	require.Contains(t, out.String(), "restored snap-1 onto feature-x")
	require.Contains(t, out.String(), "/data/snap-1.transcript")
	require.NotContains(t, out.String(), "WARNING", "no HEAD-drift warning when HEAD matches")
}

func TestPrintSnapshotRestoreConflictsAndHeadDrift(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := printSnapshotRestore(cmd, &snapshot.RestoreResult{
		SnapshotID: "snap-2", Branch: "feature-x", Applied: false, HeadMatch: false,
		SnapshotHead: "aaaaaaaa1111", CurrentHead: "bbbbbbbb2222",
		Conflicts: []string{"foo.go", "bar.go"},
	})
	require.NoError(t, err)
	s := out.String()
	require.Contains(t, s, "with conflicts")
	require.Contains(t, s, "foo.go")
	require.Contains(t, s, "bar.go")
	require.Contains(t, s, "WARNING: HEAD moved", "must warn when HEAD drifted from the snapshot")
}

// fakeSnapshotClient records calls and returns canned results — the rotate_test
// fake-client pattern applied to the snapshot verbs.
type fakeSnapshotClient struct {
	created    *snapshot.Snapshot
	createErr  error
	listResult []*snapshot.Snapshot
	restoreRes *snapshot.RestoreResult
	restoreErr error
	calls      []string
	lastForce  bool
	lastSess   string
}

func (f *fakeSnapshotClient) SnapshotCreate(_ context.Context, session, dir, message string) (*snapshot.Snapshot, error) {
	f.calls = append(f.calls, "create:"+session)
	return f.created, f.createErr
}
func (f *fakeSnapshotClient) SnapshotList(_ context.Context, session string) ([]*snapshot.Snapshot, error) {
	f.calls = append(f.calls, "list:"+session)
	f.lastSess = session
	return f.listResult, nil
}
func (f *fakeSnapshotClient) SnapshotRestore(_ context.Context, id string, force bool) (*snapshot.RestoreResult, error) {
	f.calls = append(f.calls, "restore:"+id)
	f.lastForce = force
	return f.restoreRes, f.restoreErr
}

// Compile-time check the fake satisfies the same interface the real client does.
var _ snapshotClient = (*fakeSnapshotClient)(nil)

func TestSnapshotClientInterfaceShape(t *testing.T) {
	f := &fakeSnapshotClient{
		created:    &snapshot.Snapshot{ID: "snap-1", Branch: "b"},
		restoreRes: &snapshot.RestoreResult{SnapshotID: "snap-1", Applied: true},
	}
	_, err := f.SnapshotCreate(context.Background(), "agent-1", "/wt", "m")
	require.NoError(t, err)
	_, err = f.SnapshotList(context.Background(), "agent-1")
	require.NoError(t, err)
	_, err = f.SnapshotRestore(context.Background(), "snap-1", true)
	require.NoError(t, err)
	require.Equal(t, []string{"create:agent-1", "list:agent-1", "restore:snap-1"}, f.calls)
	require.True(t, f.lastForce)
}

func TestSnapshotCreateErrorSurfaces(t *testing.T) {
	f := &fakeSnapshotClient{createErr: errors.New("snapshots disabled")}
	_, err := f.SnapshotCreate(context.Background(), "agent-1", "/wt", "")
	require.Error(t, err)
}
