package insights

import (
	"reflect"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/store"
)

var base = time.Date(2026, 6, 25, 14, 0, 0, 0, time.UTC)

// rec is a finished-session fixture helper: start at base+offset, runs durMin.
func rec(id, typ, status string, repo string, offsetMin, durMin int, files ...string) SessionRecord {
	start := base.Add(time.Duration(offsetMin) * time.Minute)
	return SessionRecord{
		ID:     id,
		Type:   typ,
		Status: status,
		Repo:   repo,
		Start:  start,
		End:    start.Add(time.Duration(durMin) * time.Minute),
		Files:  files,
	}
}

func TestDurationStatsMedianAndOutliers(t *testing.T) {
	sessions := []SessionRecord{
		rec("d1", "development", "done", "/r", 0, 10),
		rec("d2", "development", "done", "/r", 20, 10),
		rec("d3", "development", "done", "/r", 40, 12),
		rec("d4", "development", "done", "/r", 60, 60), // 6× median ⇒ outlier
		rec("a1", "analysis", "done", "/r", 0, 5),
	}
	got := durationStats(sessions, base)
	if len(got) != 2 {
		t.Fatalf("want 2 type buckets, got %d", len(got))
	}
	// Sorted by type: analysis, development.
	if got[0].Type != "analysis" || got[1].Type != "development" {
		t.Fatalf("unexpected order: %s, %s", got[0].Type, got[1].Type)
	}
	dev := got[1]
	if dev.Count != 4 {
		t.Fatalf("dev count=%d, want 4", dev.Count)
	}
	// durations sorted: 600,600,720,3600 → median (nearest-rank p50, rank 1) = 600.
	if dev.MedianSec != 600 {
		t.Fatalf("dev median=%d, want 600", dev.MedianSec)
	}
	if dev.MaxSec != 3600 {
		t.Fatalf("dev max=%d, want 3600", dev.MaxSec)
	}
	if !reflect.DeepEqual(dev.Outliers, []string{"d4"}) {
		t.Fatalf("dev outliers=%v, want [d4]", dev.Outliers)
	}
}

func TestDurationStatsSkipsActive(t *testing.T) {
	active := SessionRecord{ID: "live", Type: "code", Status: "working", Repo: "/r", Start: base} // no End
	got := durationStats([]SessionRecord{active}, base.Add(time.Hour))
	if len(got) != 0 {
		t.Fatalf("active session must not contribute to duration stats, got %v", got)
	}
}

func TestCoEditsThreshold(t *testing.T) {
	sessions := []SessionRecord{
		rec("s1", "code", "done", "/r", 0, 5, "a.go", "b.go"),
		rec("s2", "code", "done", "/r", 10, 5, "a.go", "b.go", "c.go"),
		rec("s3", "code", "done", "/r", 20, 5, "a.go"), // a.go alone, no new pair
	}
	got := coEdits(sessions)
	// (a.go,b.go) appears in s1+s2 ⇒ count 2 (surfaces). Others appear once.
	if len(got) != 1 {
		t.Fatalf("want 1 co-edit pair over threshold, got %d: %+v", len(got), got)
	}
	if got[0].A != "a.go" || got[0].B != "b.go" || got[0].Count != 2 {
		t.Fatalf("unexpected pair %+v", got[0])
	}
}

func TestErrorRatesSortedWorstFirst(t *testing.T) {
	sessions := []SessionRecord{
		rec("t1", "tests", "errored", "/r", 0, 1),
		rec("t2", "tests", "done", "/r", 1, 1),
		rec("t3", "tests", "done", "/r", 2, 1),
		rec("d1", "development", "done", "/r", 0, 1),
		rec("o1", "other", "orphaned", "/r", 0, 1),
	}
	got := errorRates(sessions)
	if got[0].Type != "other" || got[0].Rate != 1.0 {
		t.Fatalf("worst should be other@100%%, got %+v", got[0])
	}
	var tests TypeErrorRate
	for _, e := range got {
		if e.Type == "tests" {
			tests = e
		}
	}
	if tests.Total != 3 || tests.Errored != 1 {
		t.Fatalf("tests rate wrong: %+v", tests)
	}
	if d := tests.Rate; d < 0.33 || d > 0.34 {
		t.Fatalf("tests rate=%v, want ~0.333", d)
	}
}

func TestBusiestPeriods(t *testing.T) {
	mk := func(hour int) SessionRecord {
		return SessionRecord{ID: "x", Start: time.Date(2026, 6, 25, hour, 0, 0, 0, time.UTC)}
	}
	sessions := []SessionRecord{mk(14), mk(14), mk(9), mk(14), mk(9)}
	got := busiestPeriods(sessions)
	if got[0].Hour != 14 || got[0].Count != 3 {
		t.Fatalf("busiest should be 14:00×3, got %+v", got[0])
	}
	if got[1].Hour != 9 || got[1].Count != 2 {
		t.Fatalf("second should be 09:00×2, got %+v", got[1])
	}
}

func TestAnomaliesCarryForward(t *testing.T) {
	agents := []metrics.AgentSummary{
		{ID: "z", Status: "working", Anomalies: []string{"CPU pinned"}},
		{ID: "a", Status: "idle"}, // no anomalies, dropped
		{ID: "m", Status: "working", Anomalies: []string{"memory climbing"}},
	}
	got := anomalies(agents)
	if len(got) != 2 {
		t.Fatalf("want 2 anomalous agents, got %d", len(got))
	}
	if got[0].Agent != "m" || got[1].Agent != "z" {
		t.Fatalf("anomalies must sort by agent id, got %s,%s", got[0].Agent, got[1].Agent)
	}
}

func TestFromSession(t *testing.T) {
	s := &store.Session{
		ID:        "agent-1",
		Name:      "fixer",
		Type:      store.TypeDevelopment,
		Status:    store.StatusDone,
		Repo:      "/repo",
		CreatedAt: base,
		UpdatedAt: base.Add(10 * time.Minute),
	}
	r := FromSession(s, []string{"b.go", "a.go", "a.go", " "})
	if r.End.IsZero() {
		t.Fatal("finished session must carry an End")
	}
	if !reflect.DeepEqual(r.Files, []string{"a.go", "b.go"}) {
		t.Fatalf("files should be deduped+sorted, got %v", r.Files)
	}

	live := &store.Session{ID: "agent-2", Type: store.TypeCode, Status: store.StatusWorking, CreatedAt: base}
	lr := FromSession(live, nil)
	if !lr.End.IsZero() {
		t.Fatal("active session must stay open-ended (End zero)")
	}
	if lr.active() != true {
		t.Fatal("active() should be true for an open-ended session")
	}
}

func TestAnalyzeDeterministic(t *testing.T) {
	in := Input{
		Now: base.Add(2 * time.Hour),
		Sessions: []SessionRecord{
			rec("s1", "code", "done", "/r", 0, 10, "a.go"),
			rec("s2", "code", "errored", "/r", 30, 5, "b.go"),
		},
		Agents: []metrics.AgentSummary{{ID: "z", Anomalies: []string{"context climbing"}}},
	}
	r1 := Analyze(in)
	r2 := Analyze(in)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("Analyze must be deterministic for identical input")
	}
	if r1.Sessions != 2 || r1.ActiveSessions != 0 {
		t.Fatalf("session counts wrong: %d/%d", r1.Sessions, r1.ActiveSessions)
	}
	// s1+s2 are disjoint (a.go vs b.go), sequential, same repo ⇒ a suggestion.
	if len(r1.Parallelizable) != 1 {
		t.Fatalf("want 1 parallelization suggestion, got %d", len(r1.Parallelizable))
	}
}
