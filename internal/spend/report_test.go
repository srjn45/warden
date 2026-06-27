package spend

import (
	"testing"
	"time"
)

func TestBuildReportRollups(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	sessions := []SessionSpend{
		{Session: "a1", Entry: Entry{Input: 1_000_000, Output: 0, Model: "opus", Repo: "/x", Day: "2026-06-27"}},   // $5 today
		{Session: "a2", Entry: Entry{Input: 1_000_000, Output: 0, Model: "sonnet", Repo: "/x", Day: "2026-06-26"}}, // $3 yesterday (in week)
		{Session: "a3", Entry: Entry{Input: 1_000_000, Output: 0, Model: "opus", Repo: "/y", Day: "2026-06-01"}},   // $5 old (out of week)
	}
	r := BuildReport(sessions, now)

	if !approx(r.TotalUSD, 13) {
		t.Fatalf("TotalUSD = %v, want 13", r.TotalUSD)
	}
	if !approx(r.DailyUSD, 5) {
		t.Errorf("DailyUSD = %v, want 5", r.DailyUSD)
	}
	if !approx(r.WeeklyUSD, 8) {
		t.Errorf("WeeklyUSD = %v, want 8 (today $5 + yesterday $3)", r.WeeklyUSD)
	}
	// By-agent biggest-$ first: a1 ($5) and a3 ($5) tie → key order, then a2 ($3).
	if len(r.ByAgent) != 3 || r.ByAgent[0].Key != "a1" || r.ByAgent[2].Key != "a2" {
		t.Errorf("ByAgent order wrong: %+v", r.ByAgent)
	}
	// By-repo: /x = $8, /y = $5.
	if len(r.ByRepo) != 2 || r.ByRepo[0].Key != "/x" || !approx(r.ByRepo[0].USD, 8) {
		t.Errorf("ByRepo wrong: %+v", r.ByRepo)
	}
	// By-day chronological.
	if len(r.ByDay) != 3 || r.ByDay[0].Key != "2026-06-01" || r.ByDay[2].Key != "2026-06-27" {
		t.Errorf("ByDay order wrong: %+v", r.ByDay)
	}
	// AgentUSD join.
	if m := r.AgentUSD(); !approx(m["a1"], 5) || !approx(m["a2"], 3) {
		t.Errorf("AgentUSD wrong: %+v", m)
	}
}

func TestBuildReportEmptyRepoBucket(t *testing.T) {
	now := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	r := BuildReport([]SessionSpend{
		{Session: "a1", Entry: Entry{Input: 1000, Model: "opus", Day: "2026-06-27"}},
	}, now)
	if len(r.ByRepo) != 1 || r.ByRepo[0].Key != "—" {
		t.Errorf("empty repo should bucket under em-dash: %+v", r.ByRepo)
	}
}

func TestBuildReportEmpty(t *testing.T) {
	r := BuildReport(nil, time.Now())
	if r.TotalUSD != 0 || len(r.ByAgent) != 0 || len(r.ByRepo) != 0 || len(r.ByDay) != 0 {
		t.Errorf("empty input should yield empty report: %+v", r)
	}
}
