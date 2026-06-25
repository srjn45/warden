package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/metrics"
)

func TestFormatStatsSortsByRSS(t *testing.T) {
	s := &metrics.Sample{
		System: metrics.SystemStats{TotalBytes: 16 << 30, UsedBytes: 8 << 30, PressureLevel: "normal", AgentCount: 2, AttributedRSSBytes: 3 << 30},
		Agents: []metrics.AgentStat{
			{ID: "small", Paneable: true, RSSBytes: 1 << 30, CPUPercent: 1, ProcCount: 2, UptimeSec: 60},
			{ID: "big", Paneable: true, RSSBytes: 2 << 30, CPUPercent: 9, ProcCount: 3, UptimeSec: 3600},
		},
	}
	out := formatStats(s)
	// "big" must appear before "small" (sorted by RSS desc).
	if strings.Index(out, "big") > strings.Index(out, "small") {
		t.Fatalf("expected big before small:\n%s", out)
	}
	if !strings.Contains(out, "pressure") || !strings.Contains(out, "agents") {
		t.Fatalf("summary line missing:\n%s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:       "0 B",
		1023:    "1023 B",
		1 << 10: "1.0 KiB",
		1536:    "1.5 KiB",
		1 << 20: "1.0 MiB",
		2 << 30: "2.0 GiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d)=%q want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[int64]string{
		0:    "0s",
		45:   "45s",
		90:   "1m",
		3600: "1h0m",
		3725: "1h2m",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%d)=%q want %q", in, got, want)
		}
	}
}

func TestSignedBytes(t *testing.T) {
	if got := signedBytes(0); got != "flat" {
		t.Errorf("signedBytes(0)=%q want flat", got)
	}
	if got := signedBytes(2 << 20); !strings.HasPrefix(got, "↑ ") || !strings.Contains(got, "MiB") {
		t.Errorf("signedBytes(+)=%q want ↑ ...MiB", got)
	}
	if got := signedBytes(-(2 << 20)); !strings.HasPrefix(got, "↓ ") {
		t.Errorf("signedBytes(-)=%q want ↓ ...", got)
	}
}

func TestSignedTokens(t *testing.T) {
	cases := map[int]string{
		0:     "flat",
		5000:  "↑ 5k",
		-7000: "↓ 7k",
	}
	for in, want := range cases {
		if got := signedTokens(in); got != want {
			t.Errorf("signedTokens(%d)=%q want %q", in, got, want)
		}
	}
}

func TestFormatHistoryEmpty(t *testing.T) {
	if got := formatHistory(nil); got != "(no recorded history)\n" {
		t.Errorf("empty history = %q", got)
	}
}

func TestFormatHistory(t *testing.T) {
	sums := []metrics.AgentSummary{{
		ID:                  "code-1",
		Status:              "working",
		Samples:             12,
		RuntimeSec:          3725,
		LatestRSSBytes:      512 << 20,
		PeakRSSBytes:        1 << 30,
		RSSTrendBytes:       128 << 20,
		AvgCPUPercent:       12.5,
		PeakCPUPercent:      88.0,
		LatestContextTokens: 145000,
		PeakContextTokens:   210000,
		ContextTrendTokens:  -5000,
		PeakFilesModified:   7,
		Anomalies:           []string{"context approaching the limit"},
	}}
	out := formatHistory(sums)
	for _, want := range []string{
		"code-1 [working]", "12 samples", "runtime 1h2m",
		"mem:", "peak", "cpu:", "88.0% peak",
		"context: 145k now", "210k peak", "7 files changed",
		"⚠ context approaching the limit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatHistory missing %q:\n%s", want, out)
		}
	}
}

// TestStatsCmdJSON exercises the stats command's --json branch against a stub.
func TestStatsCmdJSON(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&metrics.Sample{
			System: metrics.SystemStats{TotalBytes: 16 << 30, UsedBytes: 8 << 30, PressureLevel: "normal"},
		})
	})
	out, err := runCLI(t, addr, "stats", "--json")
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stats --json not valid JSON: %v\n%s", err, out)
	}
}

// TestStatsCmdHumanAndHistory covers the default render and the --history path.
func TestStatsCmdHumanAndHistory(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "summary=true") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"summaries": []metrics.AgentSummary{{ID: "code-1", Status: "done", Samples: 1, RuntimeSec: 60}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(&metrics.Sample{
			System: metrics.SystemStats{TotalBytes: 16 << 30, UsedBytes: 8 << 30, PressureLevel: "normal"},
		})
	})

	out, err := runCLI(t, addr, "stats")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !strings.Contains(out, "system:") {
		t.Errorf("stats human output missing system line:\n%s", out)
	}

	out, err = runCLI(t, addr, "stats", "--history")
	if err != nil {
		t.Fatalf("stats --history: %v", err)
	}
	if !strings.Contains(out, "code-1 [done]") {
		t.Errorf("stats --history missing agent block:\n%s", out)
	}
}
