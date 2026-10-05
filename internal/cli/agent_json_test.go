package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runCLISplit runs the CLI returning stdout and stderr separately, so tests can
// assert that --json leaves stdout as a single JSON document.
func runCLISplit(t *testing.T, addr string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	full := append([]string{}, args...)
	if addr != "" {
		full = append(full, "--addr", addr)
	}
	full = append(full, "--config", t.TempDir()+"/none.yaml")
	root.SetArgs(full)
	err = root.Execute()
	return o.String(), e.String(), err
}

const jsonSession = `{"id":"worker-ab12","name":"nova","role":"worker","ai_cli":"codex","model":"gpt-x","workdir":"/repo/.worktrees/nova","status":"spawning"}`

func assertSpawnedJSON(t *testing.T, stdout string) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%q", err, stdout)
	}
	want := map[string]string{"id": "worker-ab12", "name": "nova", "role": "worker", "ai_cli": "codex", "model": "gpt-x", "workdir": "/repo/.worktrees/nova"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %q", k, got[k], v)
		}
	}
}

func TestStartJSON(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{"POST /api/v1/spawn": jsonSession}, nil, nil))
	out, _, err := runCLISplit(t, addr, "agent", "start", "--role", "worker", "--dir", t.TempDir(), "--json", "do it")
	if err != nil {
		t.Fatal(err)
	}
	assertSpawnedJSON(t, out)
}

func TestForkJSON(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{"POST /api/v1/spawn": jsonSession}, nil, nil))
	out, _, err := runCLISplit(t, addr, "fork", "src", "--json")
	if err != nil {
		t.Fatal(err)
	}
	assertSpawnedJSON(t, out)
}

func TestHandoffNewDelegateJSON(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{"POST /api/v1/spawn": jsonSession}, nil, nil))
	rf := writeResumeFile(t)
	out, _, err := runCLISplit(t, addr, "handoff", "--resume-file", rf, "--resume-prompt", "go", "--repo", t.TempDir(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	assertSpawnedJSON(t, out)
}

func TestHandoffToJSON(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/B-2": `{"id":"B-2"}`,
	}, nil, nil))
	rf := writeResumeFile(t)
	out, _, err := runCLISplit(t, addr, "handoff", "--to", "B-2", "--resume-file", rf, "--resume-prompt", "go", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v %q", err, out)
	}
	if got["to"] != "B-2" {
		t.Fatalf("to = %v", got["to"])
	}
	if _, ok := got["woke"]; !ok {
		t.Fatal("missing woke")
	}
}

func TestStopJSONReportsSteps(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, nil, nil, nil))
	out, errOut, err := runCLISplit(t, addr, "agent", "stop", "a1", "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID    string   `json:"id"`
		Steps []string `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout not JSON: %v %q (stderr %q)", err, out, errOut)
	}
	if got.ID != "a1" || strings.Join(got.Steps, ",") != "terminated,worktree removed,record cleared" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestStopJSONWithoutYesFailsInsteadOfPrompting(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, nil, nil, nil))
	out, _, err := runCLISplit(t, addr, "agent", "stop", "a1", "--json")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("want a clear --yes error, got %v", err)
	}
	if out != "" {
		t.Fatalf("stdout must stay empty, got %q", out)
	}
}

func TestStopJSONKeepWorktreeNeedsNoYes(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, nil, nil, nil))
	out, _, err := runCLISplit(t, addr, "agent", "stop", "a1", "--keep-worktree", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"terminated"`) || strings.Contains(out, "worktree removed") {
		t.Fatalf("out: %q", out)
	}
}

func TestRoleListJSON(t *testing.T) {
	out, _, err := runCLISplit(t, "", "agent", "role", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]string
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) == 0 {
		t.Fatalf("role list json: %v %q", err, out)
	}
	if rows[0]["name"] == "" {
		t.Fatalf("row missing name: %v", rows[0])
	}
}

// --yes and the hidden --confirm alias are equivalent confirmations.
func TestYesAndConfirmEquivalent(t *testing.T) {
	for _, flag := range []string{"--yes", "--confirm"} {
		for _, verb := range [][]string{{"rotate"}, {"handoff", "--retire"}} {
			t.Run(flag+"/"+strings.Join(verb, "_"), func(t *testing.T) {
				t.Setenv("AGENTCTL_SESSION_ID", "")
				t.Setenv("WARDEN_SESSION_ID", "SELF-1")
				addr := stubDaemon(t, routedDaemon(t, map[string]string{
					"GET /api/v1/sessions/SELF-1": `{"id":"SELF-1","workdir":"/w"}`,
					"POST /api/v1/spawn":          `{"id":"SUCC-1","workdir":"/w"}`,
				}, nil, nil))
				args := append(append([]string{}, verb...), flag, "--resume-file", writeResumeFile(t), "--resume-prompt", "go")
				out, err := runCLI(t, addr, args...)
				if err != nil || !strings.Contains(out, "successor SUCC-1") {
					t.Fatalf("%v: err=%v out=%q", args, err, out)
				}
			})
		}
	}
}

func TestRetireWithoutYesErrorsMentionsYes(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "SELF-1")
	_, err := runCLI(t, "", "rotate", "--resume-file", writeResumeFile(t), "--resume-prompt", "go")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("got %v", err)
	}
}
