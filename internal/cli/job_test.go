package cli

import (
	"strings"
	"testing"
)

// TestJobDoneCmdInPipeline: with WARDEN_PIPELINE_ID/JOB_ID set, `job done` posts
// the status + summary to the job's /done endpoint and reports it.
func TestJobDoneCmdInPipeline(t *testing.T) {
	t.Setenv("WARDEN_PIPELINE_ID", "demo")
	t.Setenv("WARDEN_JOB_ID", "a")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))

	out, err := runCLI(t, addr, "job", "done", "--summary", "shipped it")
	if err != nil {
		t.Fatalf("job done: %v", err)
	}
	if !strings.Contains(out, "done demo/a (success)") {
		t.Fatalf("job done output: %q", out)
	}
	got := body["/api/v1/pipelines/demo/jobs/a/done"]
	if !strings.Contains(got, `"summary":"shipped it"`) || !strings.Contains(got, `"status":"success"`) {
		t.Fatalf("job done body: %q", got)
	}
}

// TestJobDoneCmdFlagsOverrideEnv: explicit --pipeline/--job/--status win.
func TestJobDoneCmdFlagsOverrideEnv(t *testing.T) {
	t.Setenv("WARDEN_PIPELINE_ID", "")
	t.Setenv("WARDEN_JOB_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))

	_, err := runCLI(t, addr, "job", "done", "--pipeline", "p2", "--job", "b", "--status", "failure", "--summary", "broke")
	if err != nil {
		t.Fatalf("job done: %v", err)
	}
	got := body["/api/v1/pipelines/p2/jobs/b/done"]
	if !strings.Contains(got, `"status":"failure"`) || !strings.Contains(got, `"summary":"broke"`) {
		t.Fatalf("job done body: %q", got)
	}
}

// TestJobDoneCmdNormalizesStatus: an unrecognized status collapses to success.
func TestJobDoneCmdNormalizesStatus(t *testing.T) {
	t.Setenv("WARDEN_PIPELINE_ID", "demo")
	t.Setenv("WARDEN_JOB_ID", "a")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, nil, nil, body))

	if _, err := runCLI(t, addr, "job", "done", "--status", "whatever", "--summary", "x"); err != nil {
		t.Fatalf("job done: %v", err)
	}
	if !strings.Contains(body["/api/v1/pipelines/demo/jobs/a/done"], `"status":"success"`) {
		t.Fatalf("status not normalized: %q", body["/api/v1/pipelines/demo/jobs/a/done"])
	}
}

// TestJobDoneCmdOutsidePipelinePrintsSentinel: with no pipeline/job context the
// command degrades gracefully — it prints the <<WARDEN_DONE>> sentinel line (the
// backstop warden watches) and exits 0 without contacting any daemon.
func TestJobDoneCmdOutsidePipeline(t *testing.T) {
	t.Setenv("WARDEN_PIPELINE_ID", "")
	t.Setenv("AGENTCTL_PIPELINE_ID", "")
	t.Setenv("WARDEN_JOB_ID", "")
	t.Setenv("AGENTCTL_JOB_ID", "")

	out, err := runCLI(t, "", "job", "done", "--summary", "standalone work", "--status", "blocked")
	if err != nil {
		t.Fatalf("job done should not error outside a pipeline: %v", err)
	}
	if !strings.Contains(out, "<<WARDEN_DONE>>") {
		t.Fatalf("expected a sentinel line, got %q", out)
	}
	if !strings.Contains(out, `"status":"blocked"`) || !strings.Contains(out, `"summary":"standalone work"`) {
		t.Fatalf("sentinel payload: %q", out)
	}
}
