package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDigest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/agent-1/digest" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"summary":"did stuff","branch":"main","turns":3,"status":"idle","files":[{"path":"a.go","added":2,"removed":1,"edited":true}]}`))
	}))
	defer ts.Close()

	d, err := New(ts.URL).Digest(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d.Summary != "did stuff" || d.Branch != "main" || d.Turns != 3 || d.Status != "idle" {
		t.Errorf("digest = %+v", d)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "a.go" || d.Files[0].Added != 2 {
		t.Errorf("files = %+v", d.Files)
	}
}

func TestClientDigestNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"session not found"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).Digest(context.Background(), "x")
	var se *StatusError
	if err == nil || !errors.As(err, &se) || se.Code != 404 {
		t.Fatalf("want 404 StatusError, got %v", err)
	}
}
