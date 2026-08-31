package daemon

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func sseServer(t *testing.T, fs *fakeStore) *Server {
	t.Helper()
	return &Server{store: fs, hub: newHub()}
}

// readEvent reads lines until a blank line, returning the joined "data:" payload.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var data []string
	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if len(data) > 0 {
				return strings.Join(data, "\n")
			}
			continue // heartbeat / comment-only block
		}
		if strings.HasPrefix(line, "data: ") {
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
}

func TestSSEInitialSnapshotThenPush(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	srv := sseServer(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/events/stream", nil)
	rec := newStreamRecorder()
	go srv.handleEventsStream(rec, req)

	r := bufio.NewReader(rec.reader())
	first := readEvent(t, r)
	require.Contains(t, first, `"A-1"`)

	// A new session + publish → second snapshot.
	fs.data["B-2"] = &store.Session{ID: "B-2", Status: store.StatusIdle}
	srv.hub.publish()
	second := readEvent(t, r)
	require.Contains(t, second, `"B-2"`)
}

// TestSSERetainsLastKnownGoodOnDegraded verifies the complete-or-error contract
// on the stream: while the active scan is degraded, no partial (or empty) snapshot
// is published — the consumer keeps its last-known-good — and a later clean scan
// resumes normally.
func TestSSERetainsLastKnownGoodOnDegraded(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	srv := sseServer(t, fs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/events/stream", nil)
	rec := newStreamRecorder()
	go srv.handleEventsStream(rec, req)

	r := bufio.NewReader(rec.reader())
	require.Contains(t, readEvent(t, r), `"A-1"`)

	// The store goes degraded and (hypothetically) gains B-2, which must NOT leak.
	fs.mu.Lock()
	fs.listErr = degradedErr()
	fs.data["B-2"] = &store.Session{ID: "B-2", Status: store.StatusIdle}
	fs.mu.Unlock()
	srv.hub.publish() // degraded → send() must emit nothing

	// The store recovers; the next publish is the first the consumer should see.
	fs.mu.Lock()
	fs.listErr = nil
	fs.data["C-3"] = &store.Session{ID: "C-3", Status: store.StatusIdle}
	fs.mu.Unlock()
	srv.hub.publish()

	// If the degraded publish had leaked a partial, this read would return it
	// first; instead the next event is the recovered, complete snapshot.
	next := readEvent(t, r)
	require.Contains(t, next, `"B-2"`)
	require.Contains(t, next, `"C-3"`)
}

func TestSSEReleasedOnServerShutdown(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	srv := &Server{store: fs, hub: newHub(), done: make(chan struct{})}

	// A request whose context is never cancelled — the handler can only exit via
	// srv.done, which is exactly the shutdown path we want to exercise.
	req, _ := http.NewRequest(http.MethodGet, "/events/stream", nil)
	rec := newStreamRecorder()
	finished := make(chan struct{})
	go func() { srv.handleEventsStream(rec, req); close(finished) }()

	// Drain the initial snapshot so the handler is parked in its select loop.
	r := bufio.NewReader(rec.reader())
	require.Contains(t, readEvent(t, r), `"A-1"`)

	close(srv.done) // simulate the server beginning shutdown
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not return when server.done closed")
	}
}

// streamRecorder is a flushable, streaming ResponseWriter backed by an io.Pipe.
type streamRecorder struct {
	hdr  http.Header
	pw   *io.PipeWriter
	pr   *io.PipeReader
	once sync.Once
	code int
}

func newStreamRecorder() *streamRecorder {
	pr, pw := io.Pipe()
	return &streamRecorder{hdr: make(http.Header), pr: pr, pw: pw}
}
func (s *streamRecorder) Header() http.Header         { return s.hdr }
func (s *streamRecorder) WriteHeader(code int)        { s.code = code }
func (s *streamRecorder) Write(b []byte) (int, error) { return s.pw.Write(b) }
func (s *streamRecorder) Flush()                      {}
func (s *streamRecorder) reader() io.Reader           { return s.pr }
