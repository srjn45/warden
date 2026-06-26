package schedule

import (
	"errors"
	"testing"
	"time"
)

func TestNewCronAgent(t *testing.T) {
	now := time.Date(2026, 6, 26, 8, 0, 0, 0, time.UTC)
	s, err := New(Params{Name: "daily-review", Cron: "0 9 * * *", Type: "pr-review", Repo: "/r", Prompt: "review"}, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Kind != KindCron || s.Mode != ModeAgent {
		t.Fatalf("kind/mode = %s/%s, want cron/agent", s.Kind, s.Mode)
	}
	if !s.Enabled {
		t.Fatal("new schedule should be enabled")
	}
	if s.NextRun == nil {
		t.Fatal("NextRun should be set")
	}
	want := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	if !s.NextRun.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", s.NextRun, want)
	}
}

func TestNewAtPipeline(t *testing.T) {
	now := time.Date(2026, 6, 26, 8, 0, 0, 0, time.UTC)
	s, err := New(Params{Name: "once", At: "2026-06-27T09:00:00Z", Spec: "name: p\nrepo: /r\njobs: []\n"}, now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Kind != KindAt || s.Mode != ModePipeline {
		t.Fatalf("kind/mode = %s/%s, want at/pipeline", s.Kind, s.Mode)
	}
	want := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	if s.NextRun == nil || !s.NextRun.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", s.NextRun, want)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		p       Params
		wantErr bool
	}{
		{"cron agent ok", Params{Name: "a", Cron: "0 9 * * *", Prompt: "go"}, false},
		{"at agent ok", Params{Name: "a", At: "2026-06-27T09:00:00Z", Prompt: "go"}, false},
		{"both cron and at", Params{Name: "a", Cron: "0 9 * * *", At: "2026-06-27T09:00:00Z", Prompt: "go"}, true},
		{"neither cron nor at", Params{Name: "a", Prompt: "go"}, true},
		{"bad cron", Params{Name: "a", Cron: "not a cron", Prompt: "go"}, true},
		{"bad at", Params{Name: "a", At: "tomorrow", Prompt: "go"}, true},
		{"unsafe name", Params{Name: "../evil", Cron: "0 9 * * *", Prompt: "go"}, true},
		{"agent no prompt", Params{Name: "a", Cron: "0 9 * * *"}, true},
		{"typed agent no repo", Params{Name: "a", Cron: "0 9 * * *", Type: "pr-review", Prompt: "go"}, true},
		{"typed agent with repo", Params{Name: "a", Cron: "0 9 * * *", Type: "pr-review", Repo: "/r", Prompt: "go"}, false},
		{"pipeline ok", Params{Name: "a", Cron: "0 9 * * *", Spec: "name: p"}, false},
	}
	now := time.Now()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.p, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseAt(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"2026-06-27T09:00:00Z", false},
		{"2026-06-27T09:00:00", false},
		{"2026-06-27T09:00", false},
		{"2026-06-27 09:00", false},
		{"not a time", true},
		{"", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseAt(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseAt(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestNextCronNeverBackfills(t *testing.T) {
	// A daemon idle for days resumes at the next FUTURE 9am, not a replay.
	after := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	next, err := NextCron("0 9 * * *", after)
	if err != nil {
		t.Fatalf("NextCron: %v", err)
	}
	want := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Minute)

	if !Due(&Schedule{Enabled: true, NextRun: &past}, now) {
		t.Fatal("past NextRun should be due")
	}
	if !Due(&Schedule{Enabled: true, NextRun: &now}, now) {
		t.Fatal("NextRun == now should be due")
	}
	if Due(&Schedule{Enabled: true, NextRun: &future}, now) {
		t.Fatal("future NextRun should not be due")
	}
	if Due(&Schedule{Enabled: false, NextRun: &past}, now) {
		t.Fatal("disabled schedule should never be due")
	}
	if Due(&Schedule{Enabled: true, NextRun: nil}, now) {
		t.Fatal("nil NextRun should not be due")
	}
}

func TestAdvanceCronRearms(t *testing.T) {
	s := &Schedule{Kind: KindCron, Cron: "0 9 * * *", Enabled: true}
	now := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	Advance(s, now, nil)
	if !s.Enabled {
		t.Fatal("cron schedule should stay enabled after firing")
	}
	if s.LastRun == nil || !s.LastRun.Equal(now) {
		t.Fatalf("LastRun = %v, want %v", s.LastRun, now)
	}
	want := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	if s.NextRun == nil || !s.NextRun.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", s.NextRun, want)
	}
	if s.LastError != "" {
		t.Fatalf("LastError = %q, want empty", s.LastError)
	}
}

func TestAdvanceAtGoesInactive(t *testing.T) {
	at := time.Date(2026, 6, 27, 9, 0, 0, 0, time.UTC)
	s := &Schedule{Kind: KindAt, At: at.Format(time.RFC3339), Enabled: true, NextRun: &at}
	Advance(s, at, nil)
	if s.Enabled {
		t.Fatal("single-shot at schedule should be disabled after firing")
	}
	if s.NextRun != nil {
		t.Fatal("at schedule should have no NextRun after firing")
	}
}

func TestAdvanceRecordsError(t *testing.T) {
	s := &Schedule{Kind: KindCron, Cron: "0 9 * * *", Enabled: true}
	now := time.Date(2026, 6, 26, 9, 0, 0, 0, time.UTC)
	Advance(s, now, errors.New("boom"))
	if s.LastError != "boom" {
		t.Fatalf("LastError = %q, want boom", s.LastError)
	}
	// A fire error is recorded but does not stop a cron schedule re-arming.
	if !s.Enabled || s.NextRun == nil {
		t.Fatal("cron schedule should re-arm even after a fire error")
	}
}

func TestRecomputeDisabledClearsNext(t *testing.T) {
	at := time.Now()
	s := &Schedule{Kind: KindAt, At: at.Format(time.RFC3339), Enabled: false, NextRun: &at}
	if err := Recompute(s, time.Now()); err != nil {
		t.Fatalf("Recompute: %v", err)
	}
	if s.NextRun != nil {
		t.Fatal("disabled schedule should have nil NextRun after Recompute")
	}
}
