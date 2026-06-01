package daemon

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
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
func (s *streamRecorder) Header() http.Header      { return s.hdr }
func (s *streamRecorder) WriteHeader(code int)     { s.code = code }
func (s *streamRecorder) Write(b []byte) (int, error) { return s.pw.Write(b) }
func (s *streamRecorder) Flush()                   {}
func (s *streamRecorder) reader() io.Reader        { return s.pr }
