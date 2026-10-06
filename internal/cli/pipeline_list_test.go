package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const pipelineListBody = `{"pipelines":[
 {"id":"alpha","name":"alpha","status":"running","project_id":"/home/u/app","plan_id":"plan-1","schedule_name":"nightly",
  "jobs":[{"id":"a","status":"done"},{"id":"b","status":"running"},{"id":"s","status":"done","type":"span-out"}]},
 {"id":"beta","name":"beta","status":"done","jobs":[{"id":"a","status":"done"}]}]}`

// listStub records the query of the last pipelines request.
func listStub(t *testing.T, body string) (string, *string) {
	t.Helper()
	var query string
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	return addr, &query
}

func TestPipelineListTable(t *testing.T) {
	addr, _ := listStub(t, pipelineListBody)
	out, _, err := runCLISplit(t, addr, "pipeline", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows:\n%s", out)
	}
	for _, want := range []string{"NAME", "STATUS", "JOBS", "PROJECT", "PLAN", "SCHEDULE"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("header missing %s: %q", want, lines[0])
		}
	}
	for _, want := range []string{"alpha", "1/2", "app", "plan-1", "nightly"} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("row missing %s: %q", want, lines[1])
		}
	}
	if !strings.Contains(lines[2], "1/1") {
		t.Fatalf("row 2: %q", lines[2])
	}
}

func TestPipelineListNoScheduleColumnWithoutSchedules(t *testing.T) {
	addr, _ := listStub(t, `{"pipelines":[{"id":"b","name":"b","status":"done","jobs":[]}]}`)
	out, _, err := runCLISplit(t, addr, "pipeline", "ls", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SCHEDULE") {
		t.Fatalf("SCHEDULE shown without schedules:\n%s", out)
	}
}

func TestPipelineListScope(t *testing.T) {
	// default: the current directory's project is sent (repo root of this checkout)
	addr, q := listStub(t, `{"pipelines":[]}`)
	if _, _, err := runCLISplit(t, addr, "pipeline", "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*q, "project_id=") {
		t.Fatalf("default scope should filter by project, query=%q", *q)
	}
	// --all: no filter
	if _, _, err := runCLISplit(t, addr, "pipeline", "list", "--all"); err != nil {
		t.Fatal(err)
	}
	if *q != "" {
		t.Fatalf("--all query = %q", *q)
	}
	// --project id
	if _, _, err := runCLISplit(t, addr, "pipeline", "list", "--project", "proj-x"); err != nil {
		t.Fatal(err)
	}
	if *q != "project_id=proj-x" {
		t.Fatalf("--project query = %q", *q)
	}
}

func TestPipelineListOutsideProject(t *testing.T) {
	t.Chdir(t.TempDir())
	addr, q := listStub(t, `{"pipelines":[]}`)
	_, stderr, err := runCLISplit(t, addr, "pipeline", "list")
	if err != nil {
		t.Fatal(err)
	}
	if *q != "" || !strings.Contains(stderr, "not in a project") {
		t.Fatalf("query=%q stderr=%q", *q, stderr)
	}
}

func TestPipelineListStatus(t *testing.T) {
	addr, q := listStub(t, `{"pipelines":[]}`)
	if _, _, err := runCLISplit(t, addr, "pipeline", "list", "--all", "--status", "running,paused", "--status", "done"); err != nil {
		t.Fatal(err)
	}
	if *q != "status=running%2Cpaused%2Cdone" {
		t.Fatalf("query = %q", *q)
	}
	_, _, err := runCLISplit(t, addr, "pipeline", "list", "--all", "--status", "bogus")
	if err == nil || !strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("want validation error listing valid values, got %v", err)
	}
}

func TestPipelineListJSON(t *testing.T) {
	addr, _ := listStub(t, `{"pipelines":[]}`)
	out, _, err := runCLISplit(t, addr, "pipeline", "list", "--all", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("empty --json = %q", out)
	}
	addr, _ = listStub(t, pipelineListBody)
	out, _, err = runCLISplit(t, addr, "pipeline", "list", "--all", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 2 {
		t.Fatalf("json = %q err=%v", out, err)
	}
}

func TestPipelineListEmptyState(t *testing.T) {
	addr, _ := listStub(t, `{"pipelines":[]}`)
	out, _, err := runCLISplit(t, addr, "pipeline", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no pipelines") || !strings.Contains(out, "pipeline create <spec.yaml>") || !strings.Contains(out, "pipeline template list") {
		t.Fatalf("empty state:\n%s", out)
	}
	out, _, _ = runCLISplit(t, addr, "pipeline", "list", "--all", "--status", "running")
	if !strings.Contains(out, "status running") {
		t.Fatalf("filtered empty state should name the filter:\n%s", out)
	}
}
