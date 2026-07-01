package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsStreamingPath(t *testing.T) {
	cases := map[string]bool{
		"/events/stream":              true,
		"/sessions/abc/attach":        true,
		"/sessions/abc/messages/wait": true,
		"/metrics":                    false,
		"/sessions/abc/output":        false,
		"/sessions":                   false,
	}
	for p, want := range cases {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		if got := isStreamingPath(r); got != want {
			t.Fatalf("isStreamingPath(%q)=%v want %v", p, got, want)
		}
	}
}

func TestIsSlowPath(t *testing.T) {
	cases := map[string]bool{
		// Slow lifecycle/action routes the CLI client budgets 5min for
		// (client.longTimeout) — must NOT be cut at the 30s fast-path budget.
		"/api/v1/spawn":                        true,
		"/api/v1/adopt":                        true,
		"/api/v1/git/push":                     true,
		"/api/v1/git/sync":                     true,
		"/api/v1/check":                        true,
		"/api/v1/prune":                        true,
		"/api/v1/sessions/abc/remove-worktree": true,
		"/api/v1/sessions/abc/create-pr":       true,
		"/api/v1/sessions/abc/digest":          true,
		"/api/v1/snapshots":                    true,
		"/api/v1/snapshots/abc/restore":        true,
		"/api/v1/pipelines/p1/start":           true,
		"/api/v1/pipelines/p1/resume":          true,
		"/api/v1/pipelines/p1/jobs/j1/emit":    true,
		"/api/v1/pipelines/p1/jobs/j1/retry":   true,
		// Fast routes — the 30s guard still applies. Notably /events is the hot
		// Claude-hook ingestion path and must stay fast.
		"/api/v1/events":        false,
		"/api/v1/sessions":      false,
		"/api/v1/sessions/abc":  false,
		"/api/v1/metrics":       false,
		"/api/v1/events/stream": false,
	}
	for p, want := range cases {
		r := httptest.NewRequest(http.MethodPost, p, nil)
		if got := isSlowPath(r); got != want {
			t.Fatalf("isSlowPath(%q)=%v want %v", p, got, want)
		}
	}
}

func TestWriteTimeoutTimesOutNonStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50*time.Millisecond, 5*time.Second)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestWriteTimeoutBypassesStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50*time.Millisecond, 5*time.Second)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming path should not time out; code = %d, want 200", rec.Code)
	}
}

// A slow lifecycle route (spawn) whose handler outruns the fast budget must use
// the longer slow budget, not 503 at 30s. This is the regression from the
// slowloris write-timeout guard: spawn (git worktree checkout of a large repo)
// legitimately exceeds 30s, and the client already allots it 5 minutes.
func TestWriteTimeoutSlowPathUsesLongBudget(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50*time.Millisecond, 5*time.Second)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/spawn", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("slow path should use the long budget; code = %d, want 200", rec.Code)
	}
}

func TestMaxBytesRejectsOversizedBody(t *testing.T) {
	var readErr error
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := maxBytes(10)(handler) // 10-byte cap
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/spawn", strings.NewReader(strings.Repeat("x", 100)))
	h.ServeHTTP(rec, req)
	if readErr == nil {
		t.Fatal("expected body read to fail past the cap")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", rec.Code)
	}
}
