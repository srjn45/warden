package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
)

func getHistory(t *testing.T, srv *Server, query string) (int, sessionsResponse) {
	t.Helper()
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/history" + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr sessionsResponse
	json.NewDecoder(resp.Body).Decode(&sr)
	return resp.StatusCode, sr
}

func TestFilterClosed(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sessions := []*store.Session{
		{ID: "new", Type: store.TypeDevelopment, UpdatedAt: base.Add(48 * time.Hour)},
		{ID: "mid", Type: store.TypeAnalysis, UpdatedAt: base.Add(24 * time.Hour)},
		{ID: "old", Type: store.TypeDevelopment, UpdatedAt: base},
	}

	// since filter excludes records updated before the bound.
	got := filterClosed(sessions, base.Add(24*time.Hour), "", 0)
	if len(got) != 2 {
		t.Fatalf("since filter: want 2, got %d", len(got))
	}

	// type filter keeps only the matching type.
	got = filterClosed(sessions, time.Time{}, store.TypeDevelopment, 0)
	if len(got) != 2 || got[0].ID != "new" || got[1].ID != "old" {
		t.Fatalf("type filter: got %+v", got)
	}

	// limit caps the result, preserving order.
	got = filterClosed(sessions, time.Time{}, "", 1)
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("limit: got %+v", got)
	}

	// combined since + type.
	got = filterClosed(sessions, base.Add(time.Hour), store.TypeDevelopment, 0)
	if len(got) != 1 || got[0].ID != "new" {
		t.Fatalf("combined: got %+v", got)
	}
}

func TestHandleHistoryArchivedOnly(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	// Active session must NOT appear in history.
	fs.Insert(ctx, &store.Session{ID: "active", Status: store.StatusWorking})
	// Archived sessions appear.
	fs.Insert(ctx, &store.Session{ID: "arch1", Type: store.TypeDevelopment, UpdatedAt: time.Now()})
	fs.Archive(ctx, "arch1")
	srv := &Server{store: fs}

	code, sr := getHistory(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(sr.Sessions) != 1 || sr.Sessions[0].ID != "arch1" {
		t.Fatalf("want only arch1, got %+v", sr.Sessions)
	}
}

func TestHandleHistoryTypeFilter(t *testing.T) {
	fs := newFakeStore()
	ctx := context.Background()
	fs.Insert(ctx, &store.Session{ID: "dev", Type: store.TypeDevelopment, UpdatedAt: time.Now()})
	fs.Archive(ctx, "dev")
	fs.Insert(ctx, &store.Session{ID: "ana", Type: store.TypeAnalysis, UpdatedAt: time.Now()})
	fs.Archive(ctx, "ana")
	srv := &Server{store: fs}

	code, sr := getHistory(t, srv, "?type=development")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(sr.Sessions) != 1 || sr.Sessions[0].ID != "dev" {
		t.Fatalf("type filter: got %+v", sr.Sessions)
	}
}

func TestHandleHistoryBadSince(t *testing.T) {
	srv := &Server{store: newFakeStore()}
	code, _ := getHistory(t, srv, "?since=not-a-date")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 for bad since, got %d", code)
	}
}

func TestHandleHistoryEmptyIsNonNullArray(t *testing.T) {
	srv := &Server{store: newFakeStore()}
	code, sr := getHistory(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if sr.Sessions == nil {
		t.Fatalf("history should serialize as [], not null")
	}
}
