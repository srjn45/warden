package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/metrics"
)

func TestHandleMetricsLive(t *testing.T) {
	s := &Server{}
	s.mcollector = &metrics.Collector{
		Run: func(_ context.Context, _ string, name string, args ...string) (string, error) {
			if name == "ps" {
				return "  1  0  1024  0.0  00:05\n", nil
			}
			return "", nil
		},
		Lister:   staticLister{},
		SelfPID:  1,
		Pressure: func() string { return "normal" },
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.handleMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got metrics.Sample
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Daemon.RSSBytes != 1024*1024 {
		t.Fatalf("daemon rss=%d", got.Daemon.RSSBytes)
	}
}

func TestHandleMetricsHistory(t *testing.T) {
	dir := t.TempDir()
	r, _ := metrics.NewRecorder(dir)
	_ = r.Record(metrics.Sample{TakenAt: time.Now(), System: metrics.SystemStats{AgentCount: 7}})
	s := &Server{mrecorder: r}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics/history?limit=10", nil)
	s.handleMetricsHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Samples []metrics.Sample `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Samples) != 1 || resp.Samples[0].System.AgentCount != 7 {
		t.Fatalf("history=%+v", resp.Samples)
	}
}

func TestHandleMetricsHistoryNoRecorder(t *testing.T) {
	s := &Server{} // recorder nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics/history", nil)
	s.handleMetricsHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
}

type staticLister struct{}

func (staticLister) LiveAgents(_ context.Context) ([]metrics.Agent, error) { return nil, nil }
