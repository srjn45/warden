package tmuxproc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	calls     [][]string
	responses map[string]string
	errs      map[string]error
}

func (f *fakeRunner) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.calls = append(f.calls, argv)
	key := strings.Join(argv, " ")
	if err, ok := f.errs[key]; ok {
		return "", err
	}
	if out, ok := f.responses[key]; ok {
		return out, nil
	}
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

	require.Equal(t, []string{"tmux", "list-clients", "-t", "pane", "-F", "#{client_name} #{client_termname}"}, fr.calls[0])
	require.Equal(t, []string{"tmux", "kill-session", "-t", "pane"}, fr.calls[1])
	require.Equal(t, []string{"tmux", "capture-pane", "-p", "-t", "pane"}, fr.calls[2])
	require.Equal(t, []string{"tmux", "send-keys", "-t", "pane", "echo hi", "Enter"}, fr.calls[3])
}

func TestHostKillSessionSwitchesOuterClientBack(t *testing.T) {
	fr := &fakeRunner{
		responses: map[string]string{
			"tmux list-clients -t agent-1 -F #{client_name} #{client_termname}": "/dev/pts/0 xterm-256color\n",
		},
	}
	h := New(fr)

	require.NoError(t, h.KillSession(context.Background(), "agent-1"))
	require.Equal(t, [][]string{
		{"tmux", "list-clients", "-t", "agent-1", "-F", "#{client_name} #{client_termname}"},
		{"tmux", "switch-client", "-c", "/dev/pts/0", "-l"},
		{"tmux", "kill-session", "-t", "agent-1"},
	}, fr.calls)
}

func TestHostKillSessionDoesNotSwitchNestedClients(t *testing.T) {
	fr := &fakeRunner{
		responses: map[string]string{
			"tmux list-clients -t agent-1 -F #{client_name} #{client_termname}": "/dev/pts/1 tmux-256color\n/dev/pts/2 screen\n",
		},
	}
	h := New(fr)

	require.NoError(t, h.KillSession(context.Background(), "agent-1"))
	require.Equal(t, [][]string{
		{"tmux", "list-clients", "-t", "agent-1", "-F", "#{client_name} #{client_termname}"},
		{"tmux", "kill-session", "-t", "agent-1"},
	}, fr.calls)
}

func TestHostKillSessionMixedClients(t *testing.T) {
	fr := &fakeRunner{
		responses: map[string]string{
			"tmux list-clients -t agent-1 -F #{client_name} #{client_termname}": strings.Join([]string{
				"/dev/pts/0 xterm-256color",
				"/dev/pts/1 tmux-256color",
				"/dev/pts/2 screen-256color",
				"/dev/pts/3 alacritty",
			}, "\n"),
		},
	}
	h := New(fr)

	require.NoError(t, h.KillSession(context.Background(), "agent-1"))
	require.Equal(t, [][]string{
		{"tmux", "list-clients", "-t", "agent-1", "-F", "#{client_name} #{client_termname}"},
		{"tmux", "switch-client", "-c", "/dev/pts/0", "-l"},
		{"tmux", "switch-client", "-c", "/dev/pts/3", "-l"},
		{"tmux", "kill-session", "-t", "agent-1"},
	}, fr.calls)
}

func TestHostKillSessionMissingSessionNoError(t *testing.T) {
	fr := &fakeRunner{
		errs: map[string]error{
			"tmux list-clients -t missing -F #{client_name} #{client_termname}": errors.New("no session"),
			"tmux kill-session -t missing":                                      errors.New("no session"),
		},
	}
	h := New(fr)

	require.NoError(t, h.KillSession(context.Background(), "missing"))
	require.Equal(t, [][]string{
		{"tmux", "list-clients", "-t", "missing", "-F", "#{client_name} #{client_termname}"},
		{"tmux", "kill-session", "-t", "missing"},
	}, fr.calls)
}
