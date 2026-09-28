package cli

import (
	"strings"
	"testing"
)

const planListJSON = `{"plans":[
	{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x","file_path":"plans/pending/feature-x.yaml",
	 "status":"pending","created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"},
	{"id":"plan-ef56ab78","project_id":"proj1","name":"brain-consult","file_path":"plans/in_progress/brain-consult.yaml",
	 "status":"in_progress","execution_mode":"autopilot","created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T01:00:00Z"}
]}`

const planSingleJSON = `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x",
	"file_path":"plans/pending/feature-x.yaml","status":"pending",
	"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"}`

const scanResultJSON = `{"upserted":3}`

// planProjectID is the project ID used in plan CLI tests. Using a simple
// non-slash string avoids %2F path-encoding differences between the client's
// url.PathEscape and the test server's r.URL.Path (which decodes %2F → /).
const planProjectID = "proj1"

func TestPlanListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/projects/proj1/plans": planListJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "list", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan list: %v", err)
	}
	for _, want := range []string{"plan-ab12cd34", "feature-x", "pending", "plan-ef56ab78", "brain-consult", "in_progress"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan list missing %q: %q", want, out)
		}
	}
}

func TestPlanListCmdStatusFilter(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/projects/proj1/plans": planListJSON,
	}, nil, nil))
	// With --status, the client appends ?status=pending to the URL.
	// The stub matches only on Path (no query), so it still returns the full list.
	out, err := runCLI(t, addr, "plan", "list", "--project", planProjectID, "--status", "pending")
	if err != nil {
		t.Fatalf("plan list --status: %v", err)
	}
	// Both plans are returned by the stub; command succeeds.
	if !strings.Contains(out, "feature-x") {
		t.Fatalf("plan list --status output: %q", out)
	}
}

func TestPlanListCmdJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/projects/proj1/plans": planListJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "list", "--project", planProjectID, "--json")
	if err != nil {
		t.Fatalf("plan list --json: %v", err)
	}
	if !strings.Contains(out, `"id"`) || !strings.Contains(out, `"plan-ab12cd34"`) {
		t.Fatalf("plan list --json bad output: %q", out)
	}
}

func TestPlanShowCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/projects/proj1/plans/plan-ab12cd34": planSingleJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "show", "plan-ab12cd34", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan show: %v", err)
	}
	for _, want := range []string{"plan-ab12cd34", "feature-x", "pending", "plans/pending/feature-x.yaml"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan show missing %q: %q", want, out)
		}
	}
}

func TestPlanScanCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/proj1/plans/scan": scanResultJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "scan", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan scan: %v", err)
	}
	if !strings.Contains(out, "3 plan(s) upserted") {
		t.Fatalf("plan scan missing count: %q", out)
	}
}

func TestPlanScanCmdMigrateFlat(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/proj1/plans/scan": scanResultJSON,
	}, nil, body))
	_, err := runCLI(t, addr, "plan", "scan", "--project", planProjectID, "--migrate-flat")
	if err != nil {
		t.Fatalf("plan scan --migrate-flat: %v", err)
	}
	if !strings.Contains(body["/api/v1/projects/proj1/plans/scan"], `"migrate_flat":true`) {
		t.Fatalf("migrate_flat not forwarded: %q", body["/api/v1/projects/proj1/plans/scan"])
	}
}

func TestPlanStatusCmd(t *testing.T) {
	body := map[string]string{}
	updatedJSON := `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x",
		"file_path":"plans/in_progress/feature-x.yaml","status":"in_progress",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T02:00:00Z"}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/projects/proj1/plans/plan-ab12cd34": updatedJSON,
	}, nil, body))
	out, err := runCLI(t, addr, "plan", "status", "plan-ab12cd34", "in_progress", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan status: %v", err)
	}
	if !strings.Contains(out, "in_progress") {
		t.Fatalf("plan status output: %q", out)
	}
	if !strings.Contains(body["/api/v1/projects/proj1/plans/plan-ab12cd34"], `"status":"in_progress"`) {
		t.Fatalf("status not forwarded: %q", body["/api/v1/projects/proj1/plans/plan-ab12cd34"])
	}
}

func TestPlanArchiveCmd(t *testing.T) {
	body := map[string]string{}
	archivedJSON := `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x",
		"file_path":"plans/archived/feature-x.yaml","status":"archived",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T03:00:00Z"}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/projects/proj1/plans/plan-ab12cd34": archivedJSON,
	}, nil, body))
	out, err := runCLI(t, addr, "plan", "archive", "plan-ab12cd34", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan archive: %v", err)
	}
	if !strings.Contains(out, "archived") {
		t.Fatalf("plan archive output: %q", out)
	}
	if !strings.Contains(body["/api/v1/projects/proj1/plans/plan-ab12cd34"], `"status":"archived"`) {
		t.Fatalf("archived status not forwarded: %q", body["/api/v1/projects/proj1/plans/plan-ab12cd34"])
	}
}

func TestPlanAssessCmd(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/proj1/plans/plan-ab12cd34/assess": `{}`,
	}, seen, nil))
	_, err := runCLI(t, addr, "plan", "assess", "plan-ab12cd34", "--project", planProjectID)
	// A 501 stub response is returned by the daemon; the CLI treats non-4xx as success.
	// In the test stub we return {} (200), so no error.
	if err != nil {
		t.Fatalf("plan assess: %v", err)
	}
	if seen["/api/v1/projects/proj1/plans/plan-ab12cd34/assess"] != "POST" {
		t.Fatalf("assess not POSTed: %q", seen)
	}
}

func TestPlanRunCmdRequiresMode(t *testing.T) {
	_, err := runCLI(t, "", "plan", "run", "plan-ab12cd34", "--project", planProjectID)
	if err == nil {
		t.Fatal("expected error when --mode is missing")
	}
	if !strings.Contains(err.Error(), "--mode") {
		t.Fatalf("expected --mode mention in error: %v", err)
	}
}

func TestPlanRunCmd(t *testing.T) {
	seen := map[string]string{}
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/proj1/plans/plan-ab12cd34/run": `{}`,
	}, seen, body))
	_, err := runCLI(t, addr, "plan", "run", "plan-ab12cd34", "--mode", "autopilot", "--project", planProjectID)
	// 501 stub returns {} (200 in test), so no error.
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if seen["/api/v1/projects/proj1/plans/plan-ab12cd34/run"] != "POST" {
		t.Fatalf("run not POSTed: %q", seen)
	}
	if !strings.Contains(body["/api/v1/projects/proj1/plans/plan-ab12cd34/run"], `"mode":"autopilot"`) {
		t.Fatalf("mode not forwarded: %q", body["/api/v1/projects/proj1/plans/plan-ab12cd34/run"])
	}
}

func TestPlanStatusCmdArgCount(t *testing.T) {
	// Missing new-status argument.
	_, err := runCLI(t, "", "plan", "status", "plan-ab12cd34")
	if err == nil {
		t.Fatal("expected error when new-status is missing")
	}
}
