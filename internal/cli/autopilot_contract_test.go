package cli

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// This locks the semantic boundary that the command redesign must preserve:
// repository enablement and one registered run's lifecycle are different API
// operations, even when their user-facing verbs sound similar.
func TestAutopilotEnablementAndRunLifecycleDispatchRemainDistinct(t *testing.T) {
	methods := map[string]string{}
	bodies := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/autopilot":                  `{"enabled":false,"enabled_repos":[],"runs":[]}`,
		"POST /api/v1/autopilot/runs/ap-123/stop": `{"run_id":"ap-123","name":"demo","state":"stopped"}`,
	}, methods, bodies))

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, addr, "autopilot", "off", "--repo", repo)
	if err != nil {
		t.Fatalf("autopilot off: %v", err)
	}
	if methods["/api/v1/autopilot"] != http.MethodPost ||
		!strings.Contains(bodies["/api/v1/autopilot"], `"enabled":false`) ||
		!strings.Contains(bodies["/api/v1/autopilot"], `"repo":`) {
		t.Fatalf("repo disable dispatch changed: method=%q body=%q", methods["/api/v1/autopilot"], bodies["/api/v1/autopilot"])
	}
	if !strings.Contains(out, "autopilot disabled for ") {
		t.Fatalf("repo disable output changed: %q", out)
	}

	out, err = runCLI(t, addr, "autopilot", "stop", "ap-123")
	if err != nil {
		t.Fatalf("autopilot stop: %v", err)
	}
	if methods["/api/v1/autopilot/runs/ap-123/stop"] != http.MethodPost {
		t.Fatalf("run stop dispatch changed: %q", methods["/api/v1/autopilot/runs/ap-123/stop"])
	}
	if strings.TrimSpace(out) != "ap-123\tdemo\tstopped" {
		t.Fatalf("run stop output changed: %q", out)
	}
}
