package metrics

import (
	"fmt"
	"sort"
	"time"
)

// AgentSummary is the per-agent rollup of a window of Samples: the resource and
// history trends warden surfaces as performance history. It is pure aggregation
// over the recorded Samples — no I/O — so it's trivially testable.
type AgentSummary struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Samples    int       `json:"samples"` // how many points fed this summary
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	RuntimeSec int64     `json:"runtime_sec"` // latest wall-clock runtime

	PeakRSSBytes   uint64 `json:"peak_rss_bytes"`
	LatestRSSBytes uint64 `json:"latest_rss_bytes"`
	RSSTrendBytes  int64  `json:"rss_trend_bytes"` // latest − earliest (signed)

	AvgCPUPercent  float64 `json:"avg_cpu_percent"`
	PeakCPUPercent float64 `json:"peak_cpu_percent"`

	LatestContextTokens int `json:"latest_context_tokens"`
	PeakContextTokens   int `json:"peak_context_tokens"`
	ContextTrendTokens  int `json:"context_trend_tokens"` // latest − earliest (signed)

	PeakFilesModified int `json:"peak_files_modified"`

	Anomalies []string `json:"anomalies,omitempty"` // human-readable warnings
}

// HistoryThresholds parameterizes anomaly detection so the daemon can reuse its
// configured context bands. Zero values disable the corresponding check.
type HistoryThresholds struct {
	ContextWarn int // context tokens at/above which the trend is "approaching the limit"
	ContextCrit int // context tokens at/above which it is "critical"
}

// SummarizeAgents rolls a window of Samples (oldest→newest order not required;
// it sorts by TakenAt) into one AgentSummary per agent seen, with anomaly
// warnings attached. Agents absent from a given sample simply don't contribute
// that point. Returns summaries sorted by agent ID for stable output.
func SummarizeAgents(samples []Sample, th HistoryThresholds) []AgentSummary {
	ordered := append([]Sample(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].TakenAt.Before(ordered[j].TakenAt) })

	type acc struct {
		s         *AgentSummary
		cpuSum    float64
		firstRSS  uint64
		firstCtx  int
		haveFirst bool
	}
	accs := map[string]*acc{}
	var order []string

	for _, smp := range ordered {
		for _, a := range smp.Agents {
			ac := accs[a.ID]
			if ac == nil {
				ac = &acc{s: &AgentSummary{ID: a.ID, FirstSeen: smp.TakenAt}}
				accs[a.ID] = ac
				order = append(order, a.ID)
			}
			sum := ac.s
			sum.Status = a.Status
			sum.Samples++
			sum.LastSeen = smp.TakenAt
			if a.RuntimeSec > sum.RuntimeSec {
				sum.RuntimeSec = a.RuntimeSec
			}

			if !ac.haveFirst {
				ac.firstRSS = a.RSSBytes
				ac.firstCtx = a.ContextTokens
				ac.haveFirst = true
			}
			if a.RSSBytes > sum.PeakRSSBytes {
				sum.PeakRSSBytes = a.RSSBytes
			}
			sum.LatestRSSBytes = a.RSSBytes
			ac.cpuSum += a.CPUPercent
			if a.CPUPercent > sum.PeakCPUPercent {
				sum.PeakCPUPercent = a.CPUPercent
			}
			if a.ContextTokens > sum.PeakContextTokens {
				sum.PeakContextTokens = a.ContextTokens
			}
			sum.LatestContextTokens = a.ContextTokens
			if a.FilesModified > sum.PeakFilesModified {
				sum.PeakFilesModified = a.FilesModified
			}
		}
	}

	out := make([]AgentSummary, 0, len(order))
	for _, id := range order {
		ac := accs[id]
		sum := ac.s
		if sum.Samples > 0 {
			sum.AvgCPUPercent = ac.cpuSum / float64(sum.Samples)
		}
		sum.RSSTrendBytes = int64(sum.LatestRSSBytes) - int64(ac.firstRSS)
		sum.ContextTrendTokens = sum.LatestContextTokens - ac.firstCtx
		sum.Anomalies = detectAnomalies(sum, th)
		out = append(out, *sum)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

const (
	// memClimbBytes is the RSS growth over a window that flags a possible leak.
	memClimbBytes = 512 << 20 // 512 MiB
	// cpuPinnedPercent is the average CPU at/above which an agent looks pinned —
	// a spin/loop signature distinct from healthy bursts (caught by peak).
	cpuPinnedPercent = 90.0
)

// detectAnomalies turns a summary into zero or more human-readable warnings. It
// is conservative: each check needs ≥2 samples (so a single point can't trip a
// trend) and reports only the clear signals — climbing memory, climbing/critical
// context, and sustained high CPU.
func detectAnomalies(s *AgentSummary, th HistoryThresholds) []string {
	if s.Samples < 2 {
		return nil
	}
	var out []string
	if s.RSSTrendBytes >= memClimbBytes {
		out = append(out, "memory climbing — RSS up "+humanBytesShort(uint64(s.RSSTrendBytes))+" over the window (possible leak)")
	}
	if th.ContextCrit > 0 && s.LatestContextTokens >= th.ContextCrit {
		out = append(out, "context critical — approaching the window limit; /compact soon")
	} else if th.ContextWarn > 0 && s.LatestContextTokens >= th.ContextWarn && s.ContextTrendTokens > 0 {
		out = append(out, "context climbing toward the limit")
	}
	if s.AvgCPUPercent >= cpuPinnedPercent {
		out = append(out, "CPU pinned — sustained high CPU across the window (possible spin/loop)")
	}
	return out
}

// humanBytesShort renders a byte count as a compact human string (for warnings).
func humanBytesShort(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
