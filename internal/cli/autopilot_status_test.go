package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const autopilotStatusBody = `{"runs":[` +
	`{"run_id":"ap-1","name":"demo","state":"degraded","plan_id":"plan-9","repo":"/r","gate":"ci",` +
	`"integration_branch":"autopilot/demo","backoff":{"stage":2,"next_retry_at":"T","last_error":"boom"},` +
	`"next_step":{"action":"retry the backend ladder from the top","at":"T","owner":"guardian"},` +
	`"resting_until":"T2"}]}`

func TestAutopilotStatusShowsRunColumns(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/autopilot": autopilotStatusBody,
	}, map[string]string{}, map[string]string{}))
	out, err := runCLI(t, addr, "autopilot", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"RUN", "NAME", "STATE", "PLAN", "GATE", "BRANCH", "PROGRESS",
		"ap-1", "demo", "degraded", "plan-9", "ci", "autopilot/demo",
		"backoff: stage 2 retry T (boom)",
		"next: retry the backend ladder from the top at T (guardian)",
		"resting until: T2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// TestAutopilotHiddenRunAliasesMatchStatus covers the hidden compatibility
// aliases (`autopilot run`, `autopilot run list`, `autopilot list`).
func TestAutopilotHiddenRunAliasesMatchStatus(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/autopilot": autopilotStatusBody,
	}, map[string]string{}, map[string]string{}))
	want, err := runCLI(t, addr, "autopilot", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"autopilot", "run"}, {"autopilot", "run", "list"}, {"autopilot", "list"}} {
		got, err := runCLI(t, addr, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got != want {
			t.Errorf("%v output differs from status:\n%s\nvs\n%s", args, got, want)
		}
		cmd := findExactCommand(t, newRootCmd(), strings.Join(args, " "))
		if !cmd.Hidden {
			t.Errorf("%v should be hidden", args)
		}
	}
	help, err := executeHelp(t, "help", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"autopilot run", "autopilot list"} {
		if !strings.Contains(help, p) {
			t.Errorf("help --all missing hidden alias %q", p)
		}
	}
}

func TestAutopilotStatusAndLandJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/autopilot":       autopilotStatusBody,
		"POST /api/v1/autopilot/land": `{"sha":"abc","pr":7,"branch":"w/x","already_landed":false}`,
	}, map[string]string{}, map[string]string{}))
	out, err := runCLI(t, addr, "autopilot", "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil || st["runs"] == nil {
		t.Fatalf("status --json not raw API result: %v %q", err, out)
	}
	if _, ok := st["enabled"]; ok {
		t.Fatalf("status --json must not contain enabled: %q", out)
	}
	if _, ok := st["enabled_repos"]; ok {
		t.Fatalf("status --json must not contain enabled_repos: %q", out)
	}
	out, err = runCLI(t, addr, "autopilot", "land", "w/x", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || res["sha"] != "abc" || res["pr"] != float64(7) {
		t.Fatalf("land --json not raw API result: %v %q", err, out)
	}
}

func TestAutopilotStatusEmptyState(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/autopilot": `{"runs":[]}`,
	}, map[string]string{}, map[string]string{}))
	out, err := runCLI(t, addr, "autopilot", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no autopilot runs", "wd plan run <plan-id> --mode autopilot"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty output missing %q:\n%s", want, out)
		}
	}
}

func TestAutopilotStatusTableProgressAndRepoColumn(t *testing.T) {
	body := `{"runs":[` +
		`{"run_id":"ap-1","name":"a","state":"active","plan_id":"p1","repo":"/r1","gate":"ci","tasks":{"landed":3,"pending":2,"in_progress":1}},` +
		`{"run_id":"ap-2","name":"b","state":"active","plan_id":"p2","repo":"/r2","gate":"ci"}]}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{"GET /api/v1/autopilot": body}, map[string]string{}, map[string]string{}))
	out, err := runCLI(t, addr, "autopilot", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3/6") || !strings.Contains(out, "REPO") {
		t.Errorf("want progress 3/6 and REPO column:\n%s", out)
	}
	if strings.Contains(out, "backoff") {
		t.Errorf("healthy runs must not print backoff:\n%s", out)
	}
	// single repo → no REPO column
	one := `{"runs":[{"run_id":"ap-1","name":"a","state":"active","repo":"/r1"}]}`
	addr = stubDaemon(t, routedDaemon(t, map[string]string{"GET /api/v1/autopilot": one}, map[string]string{}, map[string]string{}))
	out, _ = runCLI(t, addr, "autopilot", "status")
	if strings.Contains(out, "REPO") {
		t.Errorf("single-repo output must not have REPO column:\n%s", out)
	}
}
