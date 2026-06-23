package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/collab"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

func newCollabServer(t *testing.T) (*Server, *fakeStore) {
	t.Helper()
	mb, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	fs := newFakeStore()
	srv := &Server{store: fs, mbox: mb, hub: newHub(), done: make(chan struct{}), collab: collab.NewMonitor(fs, mb)}
	return srv, fs
}

func getConflicts(t *testing.T, srv *Server) (int, conflictsResponse) {
	t.Helper()
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/collab/conflicts")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var cr conflictsResponse
	json.NewDecoder(resp.Body).Decode(&cr)
	return resp.StatusCode, cr
}

func TestCollabConflictsEmptyIsNonNullArray(t *testing.T) {
	srv, fs := newCollabServer(t)
	// A worktree-less session is never scanned, so git is never invoked.
	fs.Insert(context.Background(), &store.Session{ID: "a", Status: store.StatusWorking})

	code, cr := getConflicts(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if cr.Conflicts == nil {
		t.Fatalf("conflicts should serialize as [], not null")
	}
	if len(cr.Conflicts) != 0 {
		t.Fatalf("want no conflicts, got %+v", cr.Conflicts)
	}
}

func TestCollabConflictsNilMonitor(t *testing.T) {
	srv, _ := newCollabServer(t)
	srv.collab = nil // monitor disabled

	code, cr := getConflicts(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if cr.Conflicts == nil || len(cr.Conflicts) != 0 {
		t.Fatalf("disabled monitor should return [], got %+v", cr.Conflicts)
	}
}
