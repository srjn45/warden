package daemon

import (
	"bufio"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestOutputStreamSendsFrame(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking})
	fl := &fakeLife{output: "\x1b[32mok\x1b[0m internal/tui"}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/A-1/output/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// First frame is sent immediately; read until a blank-line-terminated event.
	r := bufio.NewReader(resp.Body)
	frame := readEvent(t, r) // reuse helper from sse_test.go
	require.Contains(t, frame, `internal/tui`)
	require.Contains(t, frame, `[32mok`, "ANSI escapes survive JSON framing")
	_ = time.Second
}

func TestOutputStreamNotFound(t *testing.T) {
	fs := newFakeStore()
	fl := &fakeLife{}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/nope/output/stream")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
