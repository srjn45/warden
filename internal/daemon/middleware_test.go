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

func TestWriteTimeoutTimesOutNonStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50 * time.Millisecond)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}

func TestWriteTimeoutBypassesStreaming(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := writeTimeout(50 * time.Millisecond)(slow)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/events/stream", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("streaming path should not time out; code = %d, want 200", rec.Code)
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
	req := httptest.NewRequest(http.MethodPost, "/spawn", strings.NewReader(strings.Repeat("x", 100)))
	h.ServeHTTP(rec, req)
	if readErr == nil {
		t.Fatal("expected body read to fail past the cap")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413", rec.Code)
	}
}
