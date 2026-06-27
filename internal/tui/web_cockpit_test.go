package tui

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/lifecycle"
)

// When the cockpit session already exists, EnsureWebCockpit reuses it: a single
// has-session probe, no build calls.
func TestEnsureWebCockpitReusesExisting(t *testing.T) {
	// has-session is unmatched → FakeRunner returns (\"\", nil) = success = exists.
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", false)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	require.Len(t, fr.Calls, 1, "an existing cockpit must not be rebuilt")
	require.Equal(t, []string{"tmux", "has-session", "-t", WebCockpitSession}, fr.Calls[0].Argv)
}

// When the cockpit session is absent, EnsureWebCockpit builds it (under the
// stable web session name) and returns that name.
func TestEnsureWebCockpitBuildsWhenAbsent(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{
		"tmux has-session -t " + WebCockpitSession: {Err: errors.New("no session")},
		"tmux new-session -d -s " + WebCockpitSession + " -c " + home + " -P -F #{pane_id} " + detailPlaceholderCmd(): {Out: "%0\n"},
		"tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} " + shell:                                     {Out: "%1\n"},
		"tmux split-window -v -b -l 50% -t %1 -c /work -P -F #{pane_id} " + listPaneCmd("/bin/warden", "%0"):          {Out: "%2\n"},
	}}

	sess, err := EnsureWebCockpit(context.Background(), fr, "/bin/warden", "/work", false)
	require.NoError(t, err)
	require.Equal(t, WebCockpitSession, sess)
	// Probed first, then built under the web session name.
	require.Equal(t, []string{"tmux", "has-session", "-t", WebCockpitSession}, fr.Calls[0].Argv)
	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", WebCockpitSession, "-c", home, "-P", "-F", "#{pane_id}", detailPlaceholderCmd()}, fr.Calls[1].Argv)
}
