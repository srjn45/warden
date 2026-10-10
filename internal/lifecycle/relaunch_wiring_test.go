package lifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
)

// A Claude-vocabulary mode stored on a cursor agent is invalid there ("acceptEdits"
// is not a cursor mode); relaunch must step down, never widen, and only report the
// normalization once the launch succeeded.
func cursorAgentWithForeignMode(t *testing.T) (*FakeRunner, *Lifecycle, *agentstore.Agent, *[]ModeNormalization) {
	t.Helper()
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t agent-c": {Err: errStub("no session")},
	}}
	lc := New(fr, &FakeConfig{})
	lc.ProjectsDir = t.TempDir()
	var got []ModeNormalization
	lc.OnModeNormalized = func(_ context.Context, n ModeNormalization) { got = append(got, n) }
	sess := &agentstore.Agent{
		ID: "agent-c", TmuxSession: "agent-c", AiCli: "cursor", Workdir: t.TempDir(),
		PermissionMode: "acceptEdits",
	}
	return fr, lc, sess, &got
}

func TestRestoreNormalizesStoredModeAfterSuccessfulLaunch(t *testing.T) {
	_, lc, sess, got := cursorAgentWithForeignMode(t)
	require.NoError(t, lc.Restore(context.Background(), sess))
	require.Len(t, *got, 1)
	n := (*got)[0]
	require.Equal(t, "restore", n.Path)
	require.Equal(t, "acceptEdits", n.From)
	require.Equal(t, "default", n.To)
	require.True(t, n.Persist)
	require.Equal(t, "default", sess.PermissionMode)
	require.Equal(t, OutcomeSteppedDown, n.Rationale.Outcome)
}

func TestRestoreLaunchFailureDoesNotPersistOrReport(t *testing.T) {
	fr, lc, sess, got := cursorAgentWithForeignMode(t)
	fr.FailIf = func(argv []string) error {
		if len(argv) > 1 && argv[0] == "tmux" && argv[1] == "send-keys" {
			return errors.New("send-keys boom")
		}
		return nil
	}
	require.Error(t, lc.Restore(context.Background(), sess))
	require.Empty(t, *got)
	require.Equal(t, "acceptEdits", sess.PermissionMode, "stored mode must be untouched on a failed relaunch")
}

func TestSwitchRoleNormalizesStoredModeAfterSuccessfulLaunch(t *testing.T) {
	_, lc, sess, got := cursorAgentWithForeignMode(t)
	require.NoError(t, lc.SwitchRole(context.Background(), sess))
	require.Len(t, *got, 1)
	require.Equal(t, "switch-role", (*got)[0].Path)
	require.Equal(t, "default", sess.PermissionMode)
}

func TestRestoreKeptModeEmitsNothing(t *testing.T) {
	_, lc, sess, got := cursorAgentWithForeignMode(t)
	sess.PermissionMode = "default"
	require.NoError(t, lc.Restore(context.Background(), sess))
	require.Empty(t, *got)
	require.Equal(t, "default", sess.PermissionMode)
}
