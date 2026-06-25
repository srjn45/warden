package metrics

import (
	"strings"
	"testing"
	"time"
)

func sample(t time.Time, agents ...AgentStat) Sample {
	return Sample{TakenAt: t, Agents: agents}
}

func findSummary(sums []AgentSummary, id string) (AgentSummary, bool) {
	for _, s := range sums {
		if s.ID == id {
			return s, true
		}
	}
	return AgentSummary{}, false
}

func TestSummarizeAgentsTrends(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	// Two samples, out of order on input to exercise the sort.
	samples := []Sample{
		sample(base.Add(time.Minute),
			AgentStat{ID: "a1", Status: "working", RSSBytes: 300 << 20, CPUPercent: 80, ContextTokens: 150000, FilesModified: 5, RuntimeSec: 120}),
		sample(base,
			AgentStat{ID: "a1", Status: "working", RSSBytes: 100 << 20, CPUPercent: 40, ContextTokens: 100000, FilesModified: 3, RuntimeSec: 60}),
	}
	sums := SummarizeAgents(samples, HistoryThresholds{})
	s, ok := findSummary(sums, "a1")
	if !ok {
		t.Fatal("no summary for a1")
	}
	if s.Samples != 2 {
		t.Fatalf("samples=%d, want 2", s.Samples)
	}
	if s.RuntimeSec != 120 {
		t.Fatalf("runtime=%d, want 120 (latest/max)", s.RuntimeSec)
	}
	if s.PeakRSSBytes != 300<<20 || s.LatestRSSBytes != 300<<20 {
		t.Fatalf("peak/latest RSS = %d/%d", s.PeakRSSBytes, s.LatestRSSBytes)
	}
	if s.RSSTrendBytes != int64(200<<20) {
		t.Fatalf("rss trend=%d, want %d", s.RSSTrendBytes, 200<<20)
	}
	if s.AvgCPUPercent != 60 || s.PeakCPUPercent != 80 {
		t.Fatalf("avg/peak cpu = %v/%v", s.AvgCPUPercent, s.PeakCPUPercent)
	}
	if s.LatestContextTokens != 150000 || s.PeakContextTokens != 150000 || s.ContextTrendTokens != 50000 {
		t.Fatalf("context latest/peak/trend = %d/%d/%d", s.LatestContextTokens, s.PeakContextTokens, s.ContextTrendTokens)
	}
	if s.PeakFilesModified != 5 {
		t.Fatalf("peak files=%d, want 5", s.PeakFilesModified)
	}
}

func TestDetectAnomaliesMemoryClimb(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	samples := []Sample{
		sample(base, AgentStat{ID: "a1", RSSBytes: 100 << 20}),
		sample(base.Add(time.Minute), AgentStat{ID: "a1", RSSBytes: 800 << 20}),
	}
	s, _ := findSummary(SummarizeAgents(samples, HistoryThresholds{}), "a1")
	if !hasAnomaly(s.Anomalies, "memory climbing") {
		t.Fatalf("expected memory-climb anomaly, got %v", s.Anomalies)
	}
}

func TestDetectAnomaliesContextThresholds(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	th := HistoryThresholds{ContextWarn: 200000, ContextCrit: 400000}

	// Critical wins over warn.
	crit := []Sample{
		sample(base, AgentStat{ID: "a1", ContextTokens: 410000}),
		sample(base.Add(time.Minute), AgentStat{ID: "a1", ContextTokens: 420000}),
	}
	s, _ := findSummary(SummarizeAgents(crit, th), "a1")
	if !hasAnomaly(s.Anomalies, "context critical") {
		t.Fatalf("expected context-critical, got %v", s.Anomalies)
	}

	// Climbing into the warn band (and rising) warns but isn't critical.
	warn := []Sample{
		sample(base, AgentStat{ID: "a2", ContextTokens: 210000}),
		sample(base.Add(time.Minute), AgentStat{ID: "a2", ContextTokens: 250000}),
	}
	s, _ = findSummary(SummarizeAgents(warn, th), "a2")
	if !hasAnomaly(s.Anomalies, "context climbing") {
		t.Fatalf("expected context-climbing, got %v", s.Anomalies)
	}
}

func TestDetectAnomaliesCPUPinned(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	samples := []Sample{
		sample(base, AgentStat{ID: "a1", CPUPercent: 95}),
		sample(base.Add(time.Minute), AgentStat{ID: "a1", CPUPercent: 99}),
	}
	s, _ := findSummary(SummarizeAgents(samples, HistoryThresholds{}), "a1")
	if !hasAnomaly(s.Anomalies, "CPU pinned") {
		t.Fatalf("expected CPU-pinned, got %v", s.Anomalies)
	}
}

func TestDetectAnomaliesNeedsTwoSamples(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	samples := []Sample{
		sample(base, AgentStat{ID: "a1", RSSBytes: 900 << 20, CPUPercent: 99, ContextTokens: 500000}),
	}
	s, _ := findSummary(SummarizeAgents(samples, HistoryThresholds{ContextCrit: 400000}), "a1")
	if len(s.Anomalies) != 0 {
		t.Fatalf("single sample must not trip anomalies, got %v", s.Anomalies)
	}
}

func hasAnomaly(anomalies []string, substr string) bool {
	for _, a := range anomalies {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}
