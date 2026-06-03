package daemon

import (
	"context"
	"net/http"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

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
	resp, err := http.Get(ts.URL + "/sessions/nope/attach")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAttachFoundSessionDoesNotFastReject(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1"})
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sessions/A-1/attach")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusNotFound, resp.StatusCode)
}
