package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/autopilot"
)

// The enable/on switch is a hidden no-op that points at `plan run`.
func TestAutopilotEnableOnAreHiddenNoOps(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"enable", "on", "disable", "off"} {
		if c := findExactCommand(t, root, "autopilot "+name); !c.Hidden {
			t.Errorf("autopilot %s should be hidden", name)
		}
	}
	methods := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{}, methods, map[string]string{}))
	for _, name := range []string{"enable", "on"} {
		out, err := runCLI(t, addr, "autopilot", name)
		if err != nil {
			t.Fatalf("autopilot %s: %v", name, err)
		}
		if !strings.Contains(out, "plan run <plan-id> --mode autopilot") {
			t.Errorf("autopilot %s notice missing: %q", name, out)
		}
	}
	if len(methods) != 0 {
		t.Errorf("enable/on must not call the daemon, got %v", methods)
	}
}

// disable/off pause the repo's active runs and say which.
func TestAutopilotDisableOffPauseRuns(t *testing.T) {
	for _, name := range []string{"disable", "off"} {
		methods := map[string]string{}
		bodies := map[string]string{}
		cwd := mustGetwdRoot(t)
		before := `{"enabled":true,"enabled_repos":[],"runs":[{"run_id":"ap-1","name":"demo","state":"active","repo":"` + cwd + `"}]}`
		addr := stubDaemon(t, routedDaemon(t, map[string]string{
			"GET /api/v1/autopilot":  before,
			"POST /api/v1/autopilot": strings.Replace(before, `"active"`, `"paused"`, 1),
		}, methods, bodies))
		out, err := runCLI(t, addr, "autopilot", name)
		if err != nil {
			t.Fatalf("autopilot %s: %v", name, err)
		}
		if !strings.Contains(out, "paused ap-1") || !strings.Contains(out, "deprecated") {
			t.Errorf("autopilot %s output: %q", name, out)
		}
		if !strings.Contains(bodies["/api/v1/autopilot"], `"enabled":false`) {
			t.Errorf("autopilot %s body: %q", name, bodies["/api/v1/autopilot"])
		}
	}
}

func mustGetwdRoot(t *testing.T) string {
	t.Helper()
	root, err := autopilot.NewExecEnv().GitToplevel(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
