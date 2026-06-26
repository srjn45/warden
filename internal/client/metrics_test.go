package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metrics" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		w.Write([]byte(`{"taken_at":"2026-06-09T10:00:00Z","system":{"agent_count":3,"attributed_rss_bytes":1048576},"agents":[{"id":"a","rss_bytes":1024}],"daemon":{"goroutines":5}}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	m, err := c.GetMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.System.AgentCount != 3 || len(m.Agents) != 1 || m.Agents[0].ID != "a" {
		t.Fatalf("metrics=%+v", m)
	}
}

func TestGetMetricsHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "5" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"samples":[{"system":{"agent_count":2}}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	got, err := c.GetMetricsHistory(context.Background(), "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].System.AgentCount != 2 {
		t.Fatalf("history=%+v", got)
	}
}
