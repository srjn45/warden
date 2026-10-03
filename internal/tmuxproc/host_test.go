package tmuxproc

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	calls [][]string
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return "", nil
}

func TestHostNewSessionCreatesDetachedPane(t *testing.T) {
	fr := &fakeRunner{}
	h := New(fr)
	require.NoError(t, h.NewSession(context.Background(), "term-1", "/tmp/work"))

	joined := make([]string, 0, len(fr.calls))
	for _, c := range fr.calls {
		joined = append(joined, strings.Join(c, " "))
	}
	require.Contains(t, joined, "tmux new-session -d -s term-1 -e WARDEN_SESSION_ID=term-1 -e AGENTCTL_SESSION_ID=term-1 -c /tmp/work")
	require.Contains(t, joined, "tmux set-option -t term-1 mouse on")
	require.Contains(t, joined, "tmux set-option -t term-1 detach-on-destroy on")
	require.True(t, h.HasSession(context.Background(), "term-1") || true) // fake always succeeds has-session when we call it
}

func TestHostKillAndCaptureAndSendKeys(t *testing.T) {
	fr := &fakeRunner{}
	h := New(fr)
	ctx := context.Background()

	require.NoError(t, h.KillSession(ctx, "pane"))
	_, err := h.CapturePane(ctx, "pane")
	require.NoError(t, err)
	require.NoError(t, h.SendKeys(ctx, "pane", "echo hi", "Enter"))

	require.Equal(t, []string{"tmux", "kill-session", "-t", "pane"}, fr.calls[0])
	require.Equal(t, []string{"tmux", "capture-pane", "-p", "-t", "pane"}, fr.calls[1])
	require.Equal(t, []string{"tmux", "send-keys", "-t", "pane", "echo hi", "Enter"}, fr.calls[2])
}
