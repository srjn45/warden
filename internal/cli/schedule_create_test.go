package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const schedPipelineSpec = "name: nightly\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n  - id: b\n    prompt: more\n    worktree: none\n    depends_on: [a]\n"

func schedCreateStub(t *testing.T, resp string) (string, map[string]string) {
	t.Helper()
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{"POST /api/v1/schedules": resp}, nil, body))
	return addr, body
}

// Every agent option reaches the create request, and the success line names
// what fires and when.
func TestScheduleCreateAgentFlagsReachRequest(t *testing.T) {
	dir := t.TempDir()
	addr, body := schedCreateStub(t, `{"id":"n","name":"n","kind":"cron","mode":"agent","enabled":true,"role":"reviewer","ai_cli":"claude","cwd":"`+dir+`","next_run":"2030-01-02T09:00:00Z"}`)
	out, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "0 9 * * *", "--cwd", dir,
		"--role", "reviewer", "--prompt", "go", "--aicli", "claude", "--model", "sonnet",
		"--permission-mode", "acceptEdits", "--auto-restart", "--tags", "a, b", "--tier", "tier-2", "--project", "proj-1")
	if err != nil {
		t.Fatalf("schedule create: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body["/api/v1/schedules"]), &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"model": "sonnet", "ai_cli": "claude", "permission_mode": "acceptEdits", "auto_restart": true,
		"tier": "tier-2", "project_id": "proj-1", "role": "reviewer", "cwd": dir, "prompt": "go",
	} {
		if got[k] != want {
			t.Errorf("request %s = %v, want %v (body %s)", k, got[k], want, body["/api/v1/schedules"])
		}
	}
	if tags, _ := got["tags"].([]any); len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("tags = %v", got["tags"])
	}
	if _, ok := got["type"]; ok {
		t.Errorf("deprecated type must not be sent: %s", body["/api/v1/schedules"])
	}
	for _, w := range []string{"created schedule n", "agent (reviewer, claude) in " + dir, "next run "} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q: %q", w, out)
		}
	}
}

// --ai-cli is accepted as the alias of --aicli.
func TestScheduleCreateAiCliAlias(t *testing.T) {
	dir := t.TempDir()
	addr, body := schedCreateStub(t, `{"id":"n","name":"n","kind":"at","mode":"agent","enabled":true}`)
	if _, err := runCLI(t, addr, "schedule", "create", "n", "--now", "--cwd", dir, "--prompt", "go", "--ai-cli", "codex", "--model", "m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["/api/v1/schedules"], `"ai_cli":"codex"`) {
		t.Fatalf("body: %s", body["/api/v1/schedules"])
	}
}

func TestScheduleCreateModelNeedsAiCli(t *testing.T) {
	addr, _ := schedCreateStub(t, `{}`)
	_, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "@daily", "--cwd", t.TempDir(), "--prompt", "go", "--model", "sonnet")
	if err == nil || !strings.Contains(err.Error(), "--model requires --aicli") {
		t.Fatalf("err = %v", err)
	}
}

// A pipeline combined with agent flags is an error naming each of them.
func TestScheduleCreatePipelineConflictsWithAgentFlags(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(spec, []byte(schedPipelineSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, body := schedCreateStub(t, `{}`)
	_, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "@daily", "--pipeline", spec,
		"--prompt", "x", "--role", "worker", "--model", "m", "--aicli", "claude")
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	for _, f := range []string{"--prompt", "--role", "--model", "--aicli"} {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("error should name %s: %v", f, err)
		}
	}
	if body["/api/v1/schedules"] != "" {
		t.Fatalf("nothing should be sent on a conflict: %s", body["/api/v1/schedules"])
	}
}

// A pipeline schedule validates the spec and reports the job count.
func TestScheduleCreatePipelineSuccessLine(t *testing.T) {
	spec := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(spec, []byte(schedPipelineSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	addr, _ := schedCreateStub(t, `{"id":"n","name":"n","kind":"cron","mode":"pipeline","enabled":true,"next_run":"2030-01-02T09:00:00Z"}`)
	out, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "@daily", "--pipeline", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pipeline nightly with 2 jobs") {
		t.Fatalf("output: %q", out)
	}

	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    depends_on: [ghost]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "@daily", "--pipeline", bad); err == nil || !strings.Contains(err.Error(), "invalid pipeline spec") {
		t.Fatalf("err = %v", err)
	}
}

func TestScheduleCreateJSON(t *testing.T) {
	addr, _ := schedCreateStub(t, `{"id":"n","name":"n","kind":"cron","mode":"agent","enabled":true}`)
	out, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "@daily", "--cwd", t.TempDir(), "--prompt", "go", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var sc map[string]any
	if err := json.Unmarshal([]byte(out), &sc); err != nil || sc["id"] != "n" {
		t.Fatalf("not the created schedule as JSON: %q (%v)", out, err)
	}
}

// --type is gone from schedule create.
func TestScheduleCreateNoTypeFlag(t *testing.T) {
	addr, _ := schedCreateStub(t, `{}`)
	_, err := runCLI(t, addr, "schedule", "create", "n", "--cron", "@daily", "--type", "development", "--prompt", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("err = %v", err)
	}
}
