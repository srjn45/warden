package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/metrics"
)

// getJSON drives an authenticated-free GET through the daemon router and returns
// the raw response body for the caller to unmarshal.
func getJSON(t *testing.T, s *Server, path string) []byte {
	t.Helper()
	ts := httptest.NewServer(s.router())
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: code=%d body=%s", path, resp.StatusCode, body)
	}
	return body
}

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
	var got metrics.Sample
	if err := json.Unmarshal(getJSON(t, s, "/api/v1/metrics"), &got); err != nil {
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
	var resp struct {
		Samples []metrics.Sample `json:"samples"`
	}
	if err := json.Unmarshal(getJSON(t, s, "/api/v1/metrics/history?limit=10"), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Samples) != 1 || resp.Samples[0].System.AgentCount != 7 {
		t.Fatalf("history=%+v", resp.Samples)
	}
}

func TestHandleMetricsHistorySummary(t *testing.T) {
	dir := t.TempDir()
	r, _ := metrics.NewRecorder(dir)
	base := time.Now()
	// Two samples for a1 with climbing RSS so the rollup carries a trend; a2 has
	// one stable sample.
	_ = r.Record(metrics.Sample{TakenAt: base, Agents: []metrics.AgentStat{
		{ID: "a1", Status: "working", RSSBytes: 100 << 20, ContextTokens: 410000},
		{ID: "a2", Status: "idle", RSSBytes: 50 << 20},
	}})
	_ = r.Record(metrics.Sample{TakenAt: base.Add(time.Minute), Agents: []metrics.AgentStat{
		{ID: "a1", Status: "working", RSSBytes: 800 << 20, ContextTokens: 420000},
		{ID: "a2", Status: "idle", RSSBytes: 50 << 20},
	}})
	s := &Server{mrecorder: r, mTokenWarn: 200000, mTokenCrit: 400000}

	var resp struct {
		Summaries []metrics.AgentSummary `json:"summaries"`
	}
	if err := json.Unmarshal(getJSON(t, s, "/api/v1/metrics/history?summary=true&agent=a1"), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Summaries) != 1 || resp.Summaries[0].ID != "a1" {
		t.Fatalf("want one summary for a1, got %+v", resp.Summaries)
	}
	sum := resp.Summaries[0]
	if sum.PeakRSSBytes != 800<<20 {
		t.Fatalf("peak rss=%d", sum.PeakRSSBytes)
	}
	if len(sum.Anomalies) == 0 {
		t.Fatal("expected anomalies (memory climb + context critical)")
	}
}

func TestHandleMetricsHistoryNoRecorder(t *testing.T) {
	s := &Server{} // recorder nil
	getJSON(t, s, "/api/v1/metrics/history")
}

type staticLister struct{}

func (staticLister) LiveAgents(_ context.Context) ([]metrics.Agent, error) { return nil, nil }
