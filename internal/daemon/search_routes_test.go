package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/store"
)

func getSearch(t *testing.T, srv *Server, query string) (int, sessionsResponse) {
	t.Helper()
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/search" + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr sessionsResponse
	json.NewDecoder(resp.Body).Decode(&sr)
	return resp.StatusCode, sr
}

func TestSearchSessionsFields(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Subject: "fix the auth bug"},
		{ID: "b", Prompt: "refactor the payment flow"},
		{ID: "c", Name: "authster", Type: store.TypePRReview},
		{ID: "d", LastPaneExcerpt: "running go test ./auth/..."},
	}

	// Matches subject, name, and pane excerpt (case-insensitive).
	got := searchSessions(sessions, "AUTH")
	if len(got) != 3 {
		t.Fatalf("auth: want 3 (a,c,d), got %d: %+v", len(got), got)
	}

	// Matches prompt.
	got = searchSessions(sessions, "payment")
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("payment: got %+v", got)
	}

	// AND semantics across terms: both must be present somewhere.
	got = searchSessions(sessions, "auth pr-review")
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("AND terms: got %+v", got)
	}

	// No match.
	if got = searchSessions(sessions, "nonsense"); len(got) != 0 {
		t.Fatalf("nonsense: want 0, got %+v", got)
	}

	// Blank query matches nothing.
	if got = searchSessions(sessions, "   "); len(got) != 0 {
		t.Fatalf("blank: want 0, got %+v", got)
	}
}

func TestSearchSessionsMatchesTags(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Subject: "fix login", Tags: []string{"backend", "urgent"}},
		{ID: "b", Subject: "tweak css", Tags: []string{"frontend"}},
		{ID: "c", Subject: "no tags here"},
	}

	// A bare tag term matches the tagged session.
	got := searchSessions(sessions, "backend")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("backend tag: got %+v", got)
	}

	// AND across a subject word and a tag.
	got = searchSessions(sessions, "css frontend")
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("subject+tag AND: got %+v", got)
	}

	// Untagged sessions are unaffected (no spurious matches).
	if got = searchSessions(sessions, "urgent"); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("urgent tag: got %+v", got)
	}
}

func TestHandleSearch(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	fs.Insert(ctx, &store.Session{ID: "active-auth", Subject: "auth work", Status: store.StatusWorking})
	fs.Insert(ctx, &store.Session{ID: "other", Subject: "deploy", Status: store.StatusWorking})
	srv := &Server{store: fs}

	code, sr := getSearch(t, srv, "?q=auth")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(sr.Sessions) != 1 || sr.Sessions[0].ID != "active-auth" {
		t.Fatalf("active search: got %+v", sr.Sessions)
	}
}

func TestHandleSearchIncludesClosed(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	fs.Insert(ctx, &store.Session{ID: "arch-auth", Subject: "auth login fix"})
	fs.Archive(ctx, "arch-auth")
	srv := &Server{store: fs}

	// Default (active only) misses the archived match.
	_, sr := getSearch(t, srv, "?q=auth")
	if len(sr.Sessions) != 0 {
		t.Fatalf("active-only should miss archived, got %+v", sr.Sessions)
	}
	// closed=true folds in the archived store.
	_, sr = getSearch(t, srv, "?q=auth&closed=true")
	if len(sr.Sessions) != 1 || sr.Sessions[0].ID != "arch-auth" {
		t.Fatalf("closed=true: got %+v", sr.Sessions)
	}
}

func TestHandleSearchEmptyQuery(t *testing.T) {
	srv := &Server{store: newFakeStore()}
	code, _ := getSearch(t, srv, "?q=")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for empty query, got %d", code)
	}
}
