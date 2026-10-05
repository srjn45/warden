package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

const scanResultJSON = `{"upserted":3,"skipped_canonical":0,"notice":"deprecated: plan scan is a one-release migration aid; it cannot affect canonical Plan definition, lifecycle, or execution after import. Prefer import-legacy. ScrivaDB remains sole authority."}`

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

func TestPlanCreateCmdWithAfterDeps(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans": planSingleJSON,
	}, nil, body))
	_, err := runCLI(t, addr, "plan", "create",
		"--project", planProjectID,
		"--name", "feature-x",
		"--goal", "ship it",
		"--task", "t1:first",
		"--task", "t2@t1:second",
	)
	if err != nil {
		t.Fatalf("plan create: %v", err)
	}
	posted := body["/api/v1/plans"]
	for _, want := range []string{`"id":"t2"`, `"prompt":"second"`, `"after":["t1"]`} {
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
	// id, prompt, after (blank), blank id to finish
	out, err := runCLIStdin(t, addr, "t1\ndo the work\n\n\n",
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
	if !strings.Contains(out, "3 stub(s) upserted") {
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
	if got.ID != "t1" || got.Prompt != "do the work:with a colon" || len(got.After) != 0 {
		t.Fatalf("got %+v", got)
	}
	got, err = parsePlanTaskFlag("t2@t1,a:next")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "t2" || got.Prompt != "next" || len(got.After) != 2 || got.After[0] != "t1" || got.After[1] != "a" {
		t.Fatalf("got %+v", got)
	}
	if _, err := parsePlanTaskFlag("no-colon"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := parsePlanTaskFlag("t@:x"); err == nil {
		t.Fatal("expected empty-after error")
	}
}

const planUpdatedJSON = `{"id":"plan-ab12cd34","project_id":"proj1","name":"feature-y",
	"goal":"ship faster","file_path":"plans/pending/feature-x.yaml","status":"pending","revision":2,
	"content_hash":"abc123","export_status":"none",
	"constraints":["be careful"],"done_when":["tests pass"],
	"task_summary":{"total":1,"done":0,"in_progress":0,"pending":1,"skipped":0},
	"tasks":[{"id":"t1","prompt":"do the work"}],
	"created_at":"2026-09-28T00:00:00Z","updated_at":"2026-09-28T02:00:00Z"}`

func TestPlanUpdateCmdFlags(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans/plan-ab12cd34":   planSingleJSON,
		"PATCH /api/v1/plans/plan-ab12cd34": planUpdatedJSON,
	}, nil, body))
	out, err := runCLI(t, addr, "plan", "update", "plan-ab12cd34",
		"--name", "feature-y",
		"--goal", "ship faster",
		"--constraint", "be careful",
		"--done-when", "tests pass",
	)
	if err != nil {
		t.Fatalf("plan update: %v", err)
	}
	if !strings.Contains(out, "updated plan plan-ab12cd34") || !strings.Contains(out, "rev=2") {
		t.Fatalf("plan update output: %q", out)
	}
	posted := body["/api/v1/plans/plan-ab12cd34"]
	for _, want := range []string{
		`"name":"feature-y"`,
		`"goal":"ship faster"`,
		`"constraints":["be careful"]`,
		`"done_when":["tests pass"]`,
		`"expected_revision":1`,
	} {
		if !strings.Contains(posted, want) {
			t.Fatalf("update body missing %q: %q", want, posted)
		}
	}
}

func TestPlanUpdateCmdFileAndOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.yaml")
	yamlBody := "name: from-file\ngoal: file goal\nconstraints:\n  - c1\ndone_when:\n  - d1\ntasks:\n  - id: t1\n    prompt: from file\n"
	if err := os.WriteFile(path, []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans/plan-ab12cd34":   planSingleJSON,
		"PATCH /api/v1/plans/plan-ab12cd34": planUpdatedJSON,
	}, nil, body))
	_, err := runCLI(t, addr, "plan", "update", "plan-ab12cd34",
		"--file", path,
		"--goal", "overlay goal",
	)
	if err != nil {
		t.Fatalf("plan update --file: %v", err)
	}
	posted := body["/api/v1/plans/plan-ab12cd34"]
	for _, want := range []string{
		`"name":"from-file"`,
		`"goal":"overlay goal"`,
		`"id":"t1"`,
		`"prompt":"from file"`,
		`"expected_revision":1`,
	} {
		if !strings.Contains(posted, want) {
			t.Fatalf("update --file body missing %q: %q", want, posted)
		}
	}
}

func TestPlanUpdateCmdRequiresChange(t *testing.T) {
	_, err := runCLI(t, "", "plan", "update", "plan-ab12cd34")
	if err == nil {
		t.Fatal("expected error when no update flags are set")
	}
	if !strings.Contains(err.Error(), "--file") {
		t.Fatalf("expected flag mention: %v", err)
	}
}

func TestPlanUpdateCmdConflict(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(planSingleJSON))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"plan not pending"}`))
	})
	_, err := runCLI(t, addr, "plan", "update", "plan-ab12cd34", "--goal", "nope")
	if err == nil {
		t.Fatal("expected 409 conflict error")
	}
	if !strings.Contains(err.Error(), "409") && !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestPlanEditCmd(t *testing.T) {
	prev := openEditor
	openEditor = func(_ *cobra.Command, path string) error {
		edited := "name: edited-name\ngoal: edited goal\nconstraints:\n  - c1\ndone_when:\n  - d1\ntasks:\n  - id: t1\n    prompt: edited prompt\n"
		return os.WriteFile(path, []byte(edited), 0o644)
	}
	t.Cleanup(func() { openEditor = prev })

	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans/plan-ab12cd34":   planSingleJSON,
		"PATCH /api/v1/plans/plan-ab12cd34": planUpdatedJSON,
	}, nil, body))
	out, err := runCLI(t, addr, "plan", "edit", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan edit: %v", err)
	}
	if !strings.Contains(out, "updated plan plan-ab12cd34") {
		t.Fatalf("plan edit output: %q", out)
	}
	posted := body["/api/v1/plans/plan-ab12cd34"]
	for _, want := range []string{
		`"name":"edited-name"`,
		`"goal":"edited goal"`,
		`"prompt":"edited prompt"`,
		`"expected_revision":1`,
	} {
		if !strings.Contains(posted, want) {
			t.Fatalf("edit body missing %q: %q", want, posted)
		}
	}
}

func TestPlanEditCmdUnchangedAborts(t *testing.T) {
	prev := openEditor
	openEditor = func(_ *cobra.Command, _ string) error { return nil } // leave file as-is
	t.Cleanup(func() { openEditor = prev })

	patched := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patched = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(planSingleJSON))
	})
	out, err := runCLI(t, addr, "plan", "edit", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan edit unchanged: %v", err)
	}
	if patched {
		t.Fatal("expected no PATCH when editor leaves file unchanged")
	}
	if !strings.Contains(out, "no changes") {
		t.Fatalf("expected abort message: %q", out)
	}
}

func TestPlanTaskAddCmd(t *testing.T) {
	body := map[string]string{}
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans/plan-ab12cd34/tasks": planUpdatedJSON,
	}, seen, body))
	out, err := runCLI(t, addr, "plan", "task", "add", "plan-ab12cd34",
		"--id", "t2",
		"--prompt", "second task",
		"--after", "t1",
		"--expected-revision", "1",
	)
	if err != nil {
		t.Fatalf("plan task add: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/tasks"] != "POST" {
		t.Fatalf("task add not POSTed: %q", seen)
	}
	posted := body["/api/v1/plans/plan-ab12cd34/tasks"]
	for _, want := range []string{`"id":"t2"`, `"prompt":"second task"`, `"after":["t1"]`, `"expected_revision":1`} {
		if !strings.Contains(posted, want) {
			t.Fatalf("task add body missing %q: %q", want, posted)
		}
	}
	if !strings.Contains(out, "t2") || !strings.Contains(out, "added") {
		t.Fatalf("task add output: %q", out)
	}
}

func TestPlanTaskEditCmd(t *testing.T) {
	body := map[string]string{}
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/plans/plan-ab12cd34/tasks/t1/definition": planUpdatedJSON,
	}, seen, body))
	out, err := runCLI(t, addr, "plan", "task", "edit", "plan-ab12cd34",
		"--id", "t1",
		"--prompt", "refined",
		"--after", "t0",
	)
	if err != nil {
		t.Fatalf("plan task edit: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/tasks/t1/definition"] != "PATCH" {
		t.Fatalf("task edit not PATCHed: %q", seen)
	}
	posted := body["/api/v1/plans/plan-ab12cd34/tasks/t1/definition"]
	for _, want := range []string{`"prompt":"refined"`, `"after":["t0"]`} {
		if !strings.Contains(posted, want) {
			t.Fatalf("task edit body missing %q: %q", want, posted)
		}
	}
	if !strings.Contains(out, "updated") {
		t.Fatalf("task edit output: %q", out)
	}
}

func TestPlanTaskRmCmd(t *testing.T) {
	seen := map[string]string{}
	var gotQuery string
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Method
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(planUpdatedJSON))
	})
	out, err := runCLI(t, addr, "plan", "task", "rm", "plan-ab12cd34", "t2",
		"--expected-revision", "1",
	)
	if err != nil {
		t.Fatalf("plan task rm: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/tasks/t2"] != "DELETE" {
		t.Fatalf("task rm not DELETEd: %q", seen)
	}
	if !strings.Contains(gotQuery, "expected_revision=1") {
		t.Fatalf("expected_revision query missing: %q", gotQuery)
	}
	if !strings.Contains(out, "removed") {
		t.Fatalf("task rm output: %q", out)
	}
}

func TestPlanTaskRmCmdViaFlag(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"DELETE /api/v1/plans/plan-ab12cd34/tasks/t2": planUpdatedJSON,
	}, seen, nil))
	_, err := runCLI(t, addr, "plan", "task", "rm", "plan-ab12cd34", "--id", "t2")
	if err != nil {
		t.Fatalf("plan task rm --id: %v", err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/tasks/t2"] != "DELETE" {
		t.Fatalf("task rm --id not DELETEd: %q", seen)
	}
}

func TestPlanTaskAddCmdConflict(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"revision conflict"}`))
	})
	_, err := runCLI(t, addr, "plan", "task", "add", "plan-ab12cd34",
		"--id", "t2", "--prompt", "x",
	)
	if err == nil {
		t.Fatal("expected 409 conflict error")
	}
	if !strings.Contains(err.Error(), "409") && !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestPlanDeprecatedCommandsHidden(t *testing.T) {
	hidden := []string{"import", "scan", "status"}
	help, err := executeHelp(t, "plan", "--help")
	if err != nil {
		t.Fatal(err)
	}
	all, err := executeHelp(t, "help", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range hidden {
		if strings.Contains(help, "\n  "+name+" ") {
			t.Errorf("plan --help still lists %q", name)
		}
		if !strings.Contains(all, "warden plan "+name+" ") {
			t.Errorf("help --all missing plan %s", name)
		}
	}
	if !strings.Contains(help, "import-legacy") {
		t.Error("plan --help must keep import-legacy visible")
	}
}

func TestPlanScanDeprecationNoticeOnStderrOnly(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/projects/proj1/plans/scan": scanResultJSON,
	}, nil, nil))
	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"plan", "scan", "--json", "--project", planProjectID, "--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "deprecated") {
		t.Errorf("stderr missing deprecation notice: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "warning") {
		t.Errorf("stdout polluted: %q", stdout.String())
	}
}

const planStatusPath = "/api/v1/plans/plan-ab12cd34/tasks/t1/status"

func TestPlanTaskStatusCmdAllStatuses(t *testing.T) {
	for _, st := range []string{"pending", "in_progress", "done", "skipped"} {
		t.Run(st, func(t *testing.T) {
			seen := map[string]string{}
			body := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, map[string]string{
				"GET /api/v1/plans/plan-ab12cd34": planSingleJSON,
				"POST " + planStatusPath:          planSingleJSON,
			}, seen, body))
			out, err := runCLI(t, addr, "plan", "task", "status", "plan-ab12cd34", "t1", st)
			if err != nil {
				t.Fatalf("plan task status %s: %v", st, err)
			}
			if !strings.Contains(body[planStatusPath], `"status":"`+st+`"`) {
				t.Fatalf("status not forwarded: %q", body[planStatusPath])
			}
			if !strings.Contains(out, "pending → "+st) || !strings.Contains(out, "0/1 done") {
				t.Fatalf("output: %q", out)
			}
		})
	}
}

func TestPlanTaskStatusCmdJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/plans/plan-ab12cd34": planSingleJSON,
		"POST " + planStatusPath:          planSingleJSON,
	}, map[string]string{}, map[string]string{}))
	out, err := runCLI(t, addr, "plan", "task", "status", "plan-ab12cd34", "t1", "skipped", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"old_status": "pending"`, `"new_status": "skipped"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("json missing %q: %s", want, out)
		}
	}
}

func TestPlanTaskStatusCmdInvalid(t *testing.T) {
	called := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := runCLI(t, addr, "plan", "task", "status", "plan-ab12cd34", "t1", "bogus")
	if err == nil {
		t.Fatal("expected invalid status error")
	}
	for _, want := range []string{"bogus", "pending", "in_progress", "done", "skipped"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	if called {
		t.Fatal("daemon called despite invalid status")
	}
}

func TestPlanTaskEditCmdPositional(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PATCH /api/v1/plans/plan-ab12cd34/tasks/t1/definition": planUpdatedJSON,
	}, seen, nil))
	if _, err := runCLI(t, addr, "plan", "task", "edit", "plan-ab12cd34", "t1", "--prompt", "x"); err != nil {
		t.Fatal(err)
	}
	if seen["/api/v1/plans/plan-ab12cd34/tasks/t1/definition"] != "PATCH" {
		t.Fatalf("positional edit not PATCHed: %q", seen)
	}
	// --id equal to positional is fine.
	if _, err := runCLI(t, addr, "plan", "task", "edit", "plan-ab12cd34", "t1", "--id", "t1", "--prompt", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestPlanTaskIDConflict(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, sub := range []string{"edit", "rm"} {
		args := []string{"plan", "task", sub, "plan-ab12cd34", "t1", "--id", "t2"}
		if sub == "edit" {
			args = append(args, "--prompt", "x")
		}
		_, err := runCLI(t, addr, args...)
		if err == nil || !strings.Contains(err.Error(), "conflicting task id") {
			t.Fatalf("%s: expected conflict error, got %v", sub, err)
		}
	}
}

func TestPlanDeleteCmd(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"DELETE /api/v1/plans/plan-ab12cd34": `{"status":"deleted"}`,
	}, seen, nil))
	out, err := runCLI(t, addr, "plan", "delete", "plan-ab12cd34")
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if !strings.Contains(out, "deleted") {
		t.Fatalf("plan delete output: %q", out)
	}
	if seen["/api/v1/plans/plan-ab12cd34"] != "DELETE" {
		t.Fatalf("delete not sent: %q", seen)
	}
}
