package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const autopilotStatusBody = `{"enabled":true,"enabled_repos":["/r"],"runs":[` +
	`{"run_id":"ap-1","name":"demo","state":"running","plan_id":"plan-9","repo":"/r","gate":"ci",` +
	`"integration_branch":"autopilot/demo","backoff":{"stage":2,"next_retry_at":"T","last_error":"boom"}}]}`

func TestAutopilotStatusShowsRunColumns(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/autopilot": autopilotStatusBody,
	}, map[string]string{}, map[string]string{}))
	out, err := runCLI(t, addr, "autopilot", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ap-1", "demo", "running", "plan=plan-9", "/r", "gate=ci", "branch=autopilot/demo", "backoff=stage 2 retry T (boom)"} {
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
	if err := json.Unmarshal([]byte(out), &st); err != nil || st["enabled"] != true {
		t.Fatalf("status --json not raw API result: %v %q", err, out)
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
