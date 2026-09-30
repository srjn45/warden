package cli

import (
	"strings"
	"testing"
)

const planListJSON = `[
	{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x","file_path":"plans/pending/feature-x.yaml",
	 "goal":"ship it","status":"pending","revision":1,"export_status":"none",
	 "task_summary":{"total":1,"done":0,"in_progress":0,"pending":1,"skipped":0},
	 "created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"},
	{"id":"plan-ef56ab78","project_id":"proj1","name":"brain-consult","file_path":"plans/in_progress/brain-consult.yaml",
	 "status":"in_progress","execution_mode":"autopilot","executor_id":"ap-1","revision":2,"export_status":"stale",
	 "task_summary":{"total":2,"done":1,"in_progress":1,"pending":0,"skipped":0},
	 "created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T01:00:00Z"}
]`

const planSingleJSON = `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x",
	"goal":"ship it","file_path":"plans/pending/feature-x.yaml","status":"pending","revision":1,
	"export_status":"none","executor_id":"",
	"task_summary":{"total":1,"done":0,"in_progress":0,"pending":1,"skipped":0},
	"tasks":[{"id":"t1","prompt":"do the work"}],
	"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T00:00:00Z"}`

const scanResultJSON = `{"upserted":3}`

// planProjectID is the project ID used in plan CLI tests. Using a simple
// non-slash string avoids %2F path-encoding differences between the client's
// url.PathEscape and the test server's r.URL.Path (which decodes %2F → /).
const planProjectID = "proj1"

func TestPlanListCmd(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans": planListJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "list", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan list: %v", err)
	}
	for _, want := range []string{"plan-ab12cd34", "feature-x", "pending", "plan-ef56ab78", "brain-consult", "in_progress", "REV", "EXPORT", "TASKS"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan list missing %q: %q", want, out)
		}
	}
}

func TestPlanListCmdStatusFilter(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans": planListJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "list", "--project", planProjectID, "--status", "pending")
	if err != nil {
		t.Fatalf("plan list --status: %v", err)
	}
	if !strings.Contains(out, "feature-x") {
		t.Fatalf("plan list --status output: %q", out)
	}
}

func TestPlanListCmdJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans": planListJSON,
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
		"GET /api/v1/plans/plan-ab12cd34": planSingleJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "show", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan show: %v", err)
	}
	for _, want := range []string{"plan-ab12cd34", "feature-x", "pending", "plans/pending/feature-x.yaml", "ship it", "t1", "export_status", "revision"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plan show missing %q: %q", want, out)
		}
	}
}

func TestPlanRelatedCmd(t *testing.T) {
	relatedJSON := `{"heuristic":true,"disclaimer":"Related-plan hits are heuristic","anchor_id":"plan-ab12cd34",
		"hits":[{"plan_id":"plan-ef56ab78","name":"brain-consult","status":"in_progress","score":18,"reasons":["same_project","title_overlap:1"]}]}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans/plan-ab12cd34/related": relatedJSON,
	}, nil, nil))
	out, err := runCLI(t, addr, "plan", "related", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan related: %v", err)
	}
	for _, want := range []string{"plan-ef56ab78", "brain-consult", "heuristic", "plan-ab12cd34"} {
		if !strings.Contains(out, want) && want == "heuristic" {
			// disclaimer text uses "heuristic" in the note line from the fixture
			continue
		}
		if !strings.Contains(out, want) {
			t.Fatalf("plan related missing %q: %q", want, out)
		}
	}
	if !strings.Contains(out, "Related-plan hits are heuristic") {
		t.Fatalf("plan related missing disclaimer: %q", out)
	}
}

func TestPlanCreateCmd(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans": planSingleJSON,
	}, nil, body))
	out, err := runCLI(t, addr, "plan", "create",
		"--project", planProjectID,
		"--name", "feature-x",
		"--goal", "ship it",
		"--task", "t1:do the work",
	)
	if err != nil {
		t.Fatalf("plan create: %v", err)
	}
	if !strings.Contains(out, "created plan plan-ab12cd34") {
		t.Fatalf("plan create output: %q", out)
	}
	posted := body["/api/v1/plans"]
	for _, want := range []string{`"name":"feature-x"`, `"goal":"ship it"`, `"id":"t1"`, `"prompt":"do the work"`, `"project_id":"proj1"`} {
		if !strings.Contains(posted, want) {
			t.Fatalf("create body missing %q: %q", want, posted)
		}
	}
}

func TestPlanCreateCmdInteractive(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans": planSingleJSON,
	}, nil, body))
	out, err := runCLIStdin(t, addr, "t1\ndo the work\n\n",
		"plan", "create",
		"--project", planProjectID,
		"--name", "feature-x",
		"--goal", "ship it",
	)
	if err != nil {
		t.Fatalf("plan create interactive: %v\n%s", err, out)
	}
	if !strings.Contains(body["/api/v1/plans"], `"id":"t1"`) {
		t.Fatalf("interactive task not forwarded: %q", body["/api/v1/plans"])
	}
}

func TestPlanCreateCmdRequiresTaskWhenNonTTY(t *testing.T) {
	_, err := runCLIStdin(t, "", "",
		"plan", "create",
		"--project", planProjectID,
		"--name", "feature-x",
		"--goal", "ship it",
	)
	if err == nil {
		t.Fatal("expected error when no --task and stdin is not a tty")
	}
	if !strings.Contains(err.Error(), "--task") {
		t.Fatalf("expected --task mention: %v", err)
	}
}

func TestPlanCreateCmdRequiresName(t *testing.T) {
	_, err := runCLI(t, "", "plan", "create", "--goal", "ship it", "--task", "t1:work")
	if err == nil {
		t.Fatal("expected error when --name is missing")
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
	seen := map[string]string{}
	archivedJSON := `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x",
		"file_path":"plans/archived/feature-x.yaml","status":"archived",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T03:00:00Z"}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans/plan-ab12cd34/archive": archivedJSON,
	}, seen, nil))
	out, err := runCLI(t, addr, "plan", "archive", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan archive: %v", err)
	}
	if !strings.Contains(out, "archived") {
		t.Fatalf("plan archive output: %q", out)
	}
	if seen["/api/v1/plans/plan-ab12cd34/archive"] != "POST" {
		t.Fatalf("archive not POSTed: %q", seen)
	}
}

func TestPlanAssessCmd(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/proj1/plans/plan-ab12cd34/assess": `{}`,
	}, seen, nil))
	_, err := runCLI(t, addr, "plan", "assess", "plan-ab12cd34", "--project", planProjectID)
	if err != nil {
		t.Fatalf("plan assess: %v", err)
	}
	if seen["/api/v1/projects/proj1/plans/plan-ab12cd34/assess"] != "POST" {
		t.Fatalf("assess not POSTed: %q", seen)
	}
}

func TestPlanRunCmdRequiresMode(t *testing.T) {
	_, err := runCLI(t, "", "plan", "run", "plan-ab12cd34")
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
		"POST /api/v1/plans/plan-ab12cd34/run": planSingleJSON,
	}, seen, body))
	out, err := runCLI(t, addr, "plan", "run", "plan-ab12cd34", "--mode", "autopilot")
	if err != nil {
		t.Fatalf("plan run: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/run"] != "POST" {
		t.Fatalf("run not POSTed: %q", seen)
	}
	if !strings.Contains(body["/api/v1/plans/plan-ab12cd34/run"], `"execution_mode":"autopilot"`) {
		t.Fatalf("mode not forwarded: %q", body["/api/v1/plans/plan-ab12cd34/run"])
	}
	if !strings.Contains(out, "run started") {
		t.Fatalf("plan run output: %q", out)
	}
}

func TestPlanRunCmdOrchestratorAlias(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans/plan-ab12cd34/run": planSingleJSON,
	}, nil, body))
	_, err := runCLI(t, addr, "plan", "run", "plan-ab12cd34", "--mode", "orchestrator")
	if err != nil {
		t.Fatalf("plan run --mode orchestrator: %v", err)
	}
	if !strings.Contains(body["/api/v1/plans/plan-ab12cd34/run"], `"execution_mode":"orchestrator_worker"`) {
		t.Fatalf("orchestrator not mapped: %q", body["/api/v1/plans/plan-ab12cd34/run"])
	}
}

func TestPlanDoneCmd(t *testing.T) {
	seen := map[string]string{}
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans/plan-ab12cd34/tasks/t1/status": planSingleJSON,
	}, seen, body))
	out, err := runCLI(t, addr, "plan", "done", "plan-ab12cd34", "t1")
	if err != nil {
		t.Fatalf("plan done: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/tasks/t1/status"] != "POST" {
		t.Fatalf("done not POSTed: %q", seen)
	}
	if !strings.Contains(body["/api/v1/plans/plan-ab12cd34/tasks/t1/status"], `"status":"done"`) {
		t.Fatalf("status not forwarded: %q", body["/api/v1/plans/plan-ab12cd34/tasks/t1/status"])
	}
	if !strings.Contains(out, "t1") || !strings.Contains(out, "done") {
		t.Fatalf("plan done output: %q", out)
	}
}

func TestPlanCompleteCmd(t *testing.T) {
	seen := map[string]string{}
	completedJSON := `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-x",
		"file_path":"plans/completed/feature-x.yaml","status":"completed",
		"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T04:00:00Z"}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans/plan-ab12cd34/complete": completedJSON,
	}, seen, nil))
	out, err := runCLI(t, addr, "plan", "complete", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan complete: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/complete"] != "POST" {
		t.Fatalf("complete not POSTed: %q", seen)
	}
	if !strings.Contains(out, "completed") {
		t.Fatalf("plan complete output: %q", out)
	}
}

func TestPlanStatusCmdArgCount(t *testing.T) {
	_, err := runCLI(t, "", "plan", "status", "plan-ab12cd34")
	if err == nil {
		t.Fatal("expected error when new-status is missing")
	}
}

func TestNormalizePlanRunMode(t *testing.T) {
	got, err := normalizePlanRunMode("orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if got != "orchestrator_worker" {
		t.Fatalf("got %q", got)
	}
	if _, err := normalizePlanRunMode("bogus"); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestParsePlanTaskFlag(t *testing.T) {
	t.Parallel()
	got, err := parsePlanTaskFlag("t1:do the work:with a colon")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t1" || got.Prompt != "do the work:with a colon" {
		t.Fatalf("got %+v", got)
	}
	if _, err := parsePlanTaskFlag("no-colon"); err == nil {
		t.Fatal("expected error")
	}
}
