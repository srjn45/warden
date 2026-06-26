package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/branchtrack"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/store"
)

func newBranchServer(t *testing.T) (*Server, *fakeStore) {
	t.Helper()
	mb, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	fs := newFakeStore()
	srv := &Server{store: fs, mbox: mb, hub: newHub(), done: make(chan struct{}),
		branchTracker: branchtrack.NewTracker(fs, mb, notify.New(false))}
	return srv, fs
}

func getBranches(t *testing.T, srv *Server) (int, branchesResponse) {
	t.Helper()
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/collab/branches")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var br branchesResponse
	json.NewDecoder(resp.Body).Decode(&br)
	return resp.StatusCode, br
}

func TestBranchStatusesEmptyIsNonNullArray(t *testing.T) {
	srv, fs := newBranchServer(t)
	// A branchless/worktree-less session is never scanned, so gh/git is never invoked.
	fs.Insert(context.Background(), &store.Session{ID: "a", Status: store.StatusWorking})

	code, br := getBranches(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if br.Branches == nil {
		t.Fatalf("branches should serialize as [], not null")
	}
	if len(br.Branches) != 0 {
		t.Fatalf("want no branches, got %+v", br.Branches)
	}
}

func TestBranchStatusesNilTracker(t *testing.T) {
	srv, _ := newBranchServer(t)
	srv.branchTracker = nil // feature disabled

	code, br := getBranches(t, srv)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if br.Branches == nil || len(br.Branches) != 0 {
		t.Fatalf("disabled tracker should return [], got %+v", br.Branches)
	}
}
