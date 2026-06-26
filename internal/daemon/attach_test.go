package daemon

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestAttachEnvForcesXterm(t *testing.T) {
	// A daemon launched by launchd has no TERM; one is added so tmux can attach.
	out := attachEnv([]string{"PATH=/bin", "FOO=bar"})
	require.Contains(t, out, "TERM=xterm-256color")
	require.Contains(t, out, "PATH=/bin")

	// Any inherited TERM is overridden — the rendering endpoint is always xterm.js
	// — and exactly one TERM entry remains.
	out = attachEnv([]string{"TERM=tmux-256color", "PATH=/bin"})
	require.Contains(t, out, "TERM=xterm-256color")
	require.NotContains(t, out, "TERM=tmux-256color")
	n := 0
	for _, e := range out {
		if strings.HasPrefix(e, "TERM=") {
			n++
		}
	}
	require.Equal(t, 1, n, "exactly one TERM entry")
}

func TestParseResize(t *testing.T) {
	c, r, ok := parseResize([]byte(`{"cols":120,"rows":40}`))
	require.True(t, ok)
	require.EqualValues(t, 120, c)
	require.EqualValues(t, 40, r)

	_, _, ok = parseResize([]byte(`{"cols":0,"rows":40}`))
	require.False(t, ok, "zero cols is invalid")

	_, _, ok = parseResize([]byte(`{"rows":40}`))
	require.False(t, ok, "missing cols defaults to 0 → invalid")

	_, _, ok = parseResize([]byte(`not json`))
	require.False(t, ok, "malformed JSON is invalid")
}

func TestAttachUnknownSessionIs404(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/nope/attach")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAttachFoundSessionDoesNotFastReject(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1"})
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/A-1/attach")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusNotFound, resp.StatusCode)
}
