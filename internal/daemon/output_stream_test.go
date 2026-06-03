package daemon

import (
	"bufio"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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
}

func TestOutputStreamReleasedOnServerShutdown(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking})
	fl := &fakeLife{output: "hello"}
	srv := &Server{store: fs, life: fl, done: make(chan struct{})}

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "A-1")
	req, _ := http.NewRequest(http.MethodGet, "/sessions/A-1/output/stream", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	rec := newStreamRecorder()
	finished := make(chan struct{})
	go func() { srv.handleOutputStream(rec, req); close(finished) }()

	r := bufio.NewReader(rec.reader())
	require.Contains(t, readEvent(t, r), "hello")

	close(srv.done) // simulate the server beginning shutdown
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("output stream handler did not return when server.done closed")
	}
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
