package cli

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/schedule"
)

const (
	recurringOK     = `{"id":"morning","name":"morning","kind":"cron","mode":"agent","cron":"0 9 * * *","enabled":true,"role":"reviewer","prompt":"review PRs","cwd":"/work","ai_cli":"claude","created_at":"2026-10-01T08:00:00Z","next_run":"2099-01-02T09:00:00Z","last_run":"2026-10-05T09:00:00Z","last_run_session_id":"agent-7","last_run_status":"exited"}`
	recurringFailed = `{"id":"flaky","name":"flaky","kind":"cron","mode":"agent","cron":"@hourly","enabled":true,"prompt":"p","repo":"/r","created_at":"2026-10-01T08:00:00Z","next_run":"2099-01-02T09:00:00Z","last_run":"2026-10-05T09:00:00Z","last_error":"cwd is not an existing directory"}`
	onceDone        = `{"id":"once","name":"once","kind":"at","mode":"agent","at":"2026-10-05T09:00:00Z","enabled":false,"prompt":"p","cwd":"/w","created_at":"2026-10-01T08:00:00Z","last_run":"2026-10-05T09:00:00Z","last_run_session_id":"agent-9"}`
	onceFailed      = `{"id":"bad","name":"bad","kind":"at","mode":"agent","at":"2026-10-05T09:00:00Z","enabled":false,"prompt":"p","cwd":"/w","created_at":"2026-10-01T08:00:00Z","last_run":"2026-10-05T09:00:00Z","last_error":"no such role"}`
	switchedOff     = `{"id":"off","name":"off","kind":"cron","mode":"agent","cron":"@daily","enabled":false,"disabled":true,"prompt":"p","cwd":"/w","created_at":"2026-10-01T08:00:00Z"}`
	pipeSched       = `{"id":"nightly","name":"nightly","kind":"cron","mode":"pipeline","cron":"@daily","enabled":true,"spec":"name: nightly\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n","created_at":"2026-10-01T08:00:00Z","last_run":"2026-10-05T09:00:00Z","last_run_session_id":"nightly-20261005-090000","last_run_status":"done"}`
)

func scheduleListStub(t *testing.T, items ...string) string {
	t.Helper()
	return stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/schedules": `{"schedules":[` + strings.Join(items, ",") + `]}`,
	}, nil, nil))
}

func TestScheduleStateDerivation(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		sc   schedule.Schedule
		want schedule.State
	}{
		{"enabled", schedule.Schedule{Kind: schedule.KindCron, Enabled: true}, schedule.StateEnabled},
		{"recurring failed stays enabled", schedule.Schedule{Kind: schedule.KindCron, Enabled: true, LastRun: &now, LastError: "x"}, schedule.StateEnabled},
		{"disabled by user", schedule.Schedule{Kind: schedule.KindCron, Disabled: true}, schedule.StateDisabled},
		{"single-shot disabled before firing", schedule.Schedule{Kind: schedule.KindAt}, schedule.StateDisabled},
		{"single-shot disabled after firing", schedule.Schedule{Kind: schedule.KindAt, Disabled: true, LastRun: &now}, schedule.StateDisabled},
		{"single-shot done", schedule.Schedule{Kind: schedule.KindAt, LastRun: &now}, schedule.StateDone},
		{"single-shot failed", schedule.Schedule{Kind: schedule.KindAt, LastRun: &now, LastError: "x"}, schedule.StateFailed},
	}
	for _, c := range cases {
		if got := c.sc.State(); got != c.want {
			t.Errorf("%s: state = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestScheduleListTable(t *testing.T) {
	addr := scheduleListStub(t, recurringOK, recurringFailed, onceDone, onceFailed, switchedOff, pipeSched)
	out, err := runCLI(t, addr, "schedule", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[0], "STATE") || !strings.Contains(lines[0], "LAST") {
		t.Fatalf("header: %q", lines[0])
	}
	find := func(prefix string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		t.Fatalf("no row for %q in:\n%s", prefix, out)
		return ""
	}
	for prefix, wants := range map[string][]string{
		"morning": {"enabled", "0 9 * * *", "agent (reviewer)", "in ", "exited"},
		"flaky":   {"enabled", "agent (worker)", "failed"},
		"once":    {"done", "agent (general)"},
		"bad":     {"failed"},
		"off":     {"disabled", "@daily"},
		"nightly": {"enabled", "pipeline nightly", "done"},
	} {
		row := find(prefix)
		for _, w := range wants {
			if !strings.Contains(row, w) {
				t.Errorf("row %q missing %q", row, w)
			}
		}
	}
	if !strings.Contains(out, "\n  error: cwd is not an existing directory\n") {
		t.Errorf("error line missing:\n%s", out)
	}
	// every column starts at the same offset across rows
	col := strings.Index(lines[0], "STATE")
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "  error:") {
			continue
		}
		if len(l) < col || l[col-1] != ' ' {
			t.Errorf("STATE column misaligned in %q", l)
		}
	}
}

func TestScheduleListEmptyAndAlias(t *testing.T) {
	addr := scheduleListStub(t)
	out, err := runCLI(t, addr, "schedule", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no schedules") || !strings.Contains(out, "wd schedule create") {
		t.Fatalf("empty output: %q", out)
	}
}

func TestScheduleListJSON(t *testing.T) {
	out, err := runCLI(t, scheduleListStub(t), "schedule", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty json = %q", out)
	}
	out, err = runCLI(t, scheduleListStub(t, onceDone), "schedule", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"id": "once"`) || !strings.Contains(out, `"state": "done"`) {
		t.Fatalf("json = %q", out)
	}
}

func TestScheduleSchedulerDisabledMessage(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"scheduler disabled (enable with scheduler_enabled: true in the config file)"}`))
	})
	for _, args := range [][]string{{"schedule", "list"}, {"schedule", "show", "x"}} {
		_, err := runCLI(t, addr, args...)
		if err == nil || !strings.Contains(err.Error(), "scheduler_enabled: true") || !strings.Contains(err.Error(), "restart the daemon") || strings.Contains(err.Error(), "daemon error") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}

func showStub(t *testing.T, body string) string {
	return stubDaemon(t, routedDaemon(t, map[string]string{"GET /api/v1/schedules/x": body}, nil, nil))
}

func TestScheduleShowAgent(t *testing.T) {
	out, err := runCLI(t, showStub(t, recurringOK), "schedule", "show", "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"name:", "morning", "state:", "enabled", "cron 0 9 * * *", "next run:", "created:",
		"fires:", "agent", "review PRs", "/work", "reviewer", "claude", "last run:", "agent-7", "exited", "wd status agent-7"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func TestScheduleShowFailedRecurringIsFlagged(t *testing.T) {
	out, err := runCLI(t, showStub(t, recurringFailed), "schedule", "show", "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "enabled (last run failed)") || !strings.Contains(out, "error:") || !strings.Contains(out, "cwd is not an existing directory") {
		t.Fatalf("out:\n%s", out)
	}
}

func TestScheduleShowStates(t *testing.T) {
	for body, want := range map[string]string{onceDone: "done", onceFailed: "failed", switchedOff: "disabled"} {
		out, err := runCLI(t, showStub(t, body), "schedule", "show", "x")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "state:      "+want) {
			t.Errorf("want state %s in:\n%s", want, out)
		}
	}
}

func TestScheduleShowPipeline(t *testing.T) {
	out, err := runCLI(t, showStub(t, pipeSched), "schedule", "show", "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"pipeline", "jobs:", "1", "wd pipeline show nightly-20261005-090000"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Contains(out, "worktree: none") {
		t.Errorf("spec must be hidden without --spec:\n%s", out)
	}
	out, err = runCLI(t, showStub(t, pipeSched), "schedule", "show", "x", "--spec")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "worktree: none") {
		t.Errorf("--spec should print the YAML:\n%s", out)
	}
}

func TestScheduleShowNeverRunAndJSON(t *testing.T) {
	never := `{"id":"n","name":"n","kind":"at","mode":"agent","at":"2099-01-01T09:00:00Z","enabled":true,"prompt":"p","created_at":"2026-10-01T08:00:00Z","next_run":"2099-01-01T09:00:00Z"}`
	out, err := runCLI(t, showStub(t, never), "schedule", "show", "x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "last run:   never") || !strings.Contains(out, "once, at") {
		t.Fatalf("out:\n%s", out)
	}
	out, err = runCLI(t, showStub(t, never), "schedule", "show", "x", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"state": "enabled"`) {
		t.Fatalf("json:\n%s", out)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		30 * time.Second: "just now", 3 * time.Hour: "in 3h", -2 * time.Hour: "2h ago",
		10 * time.Minute: "in 10m", 72 * time.Hour: "in 3d",
	} {
		if got := relativeTime(now.Add(d), now); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}
