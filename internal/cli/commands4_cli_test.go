package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPipelineValidateCmd(t *testing.T) {
	spec := "name: demo\nrepo: /r\njobs:\n  - id: a\n    prompt: do x\n"
	p := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(p, []byte(spec), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	out, err := runCLI(t, "", "pipeline", "validate", "-f", p)
	if err != nil {
		t.Fatalf("pipeline validate: %v", err)
	}
	if !strings.Contains(out, "is valid") || !strings.Contains(out, `"demo"`) {
		t.Fatalf("validate output: %q", out)
	}
}

func TestPipelineValidateCmdRejectsBadSpec(t *testing.T) {
	// A dependency on a non-existent job is rejected locally (no daemon needed).
	spec := "name: demo\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    depends_on: [ghost]\n"
	p := filepath.Join(t.TempDir(), "spec.yaml")
	if err := os.WriteFile(p, []byte(spec), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if _, err := runCLI(t, "", "pipeline", "validate", "-f", p); err == nil {
		t.Fatal("a spec with a dangling dependency must fail validation")
	}
}

func TestPipelineValidateCmdRequiresFile(t *testing.T) {
	if _, err := runCLI(t, "", "pipeline", "validate"); err == nil {
		t.Fatal("validate without -f must error")
	}
}

func TestPipelineListTemplatesCmd(t *testing.T) {
	out, err := runCLI(t, "", "pipeline", "list-templates")
	if err != nil {
		t.Fatalf("list-templates: %v", err)
	}
	// The built-in templates ship with the binary; at least one well-known one.
	if !strings.Contains(out, "analyze-implement-review") {
		t.Fatalf("list-templates output: %q", out)
	}
}

func TestPipelineCreateFromTemplate(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /pipelines": `{"id":"mine","name":"mine","jobs":[{"id":"a"}]}`,
	}, nil, body))
	out, err := runCLI(t, addr, "pipeline", "create", "--template", "analyze-implement-review",
		"--name", "mine", "--repo", t.TempDir(), "--set", "TASK=refactor auth")
	if err != nil {
		t.Fatalf("create from template: %v", err)
	}
	if !strings.Contains(out, "created pipeline mine") {
		t.Fatalf("create output: %q", out)
	}
	if !strings.Contains(body["/pipelines"], `"spec"`) {
		t.Fatalf("rendered spec not posted: %q", body["/pipelines"])
	}
}

func TestPipelineCreateCmdRejectsBothSources(t *testing.T) {
	if _, err := runCLI(t, "", "pipeline", "create", "-f", "x.yaml", "--template", "t"); err == nil {
		t.Fatal("create must reject both -f and --template")
	}
}

func TestCtxSetFromStdin(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"PUT /context/k": `{"key":"k","value":"piped","updated_by":"human"}`,
	}, nil, body))
	// Drive the command with stdin supplying the value.
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("piped value"))
	root.SetArgs([]string{"ctx", "set", "k", "--stdin", "--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	if err := root.Execute(); err != nil {
		t.Fatalf("ctx set --stdin: %v", err)
	}
	if !strings.Contains(body["/context/k"], `"value":"piped value"`) {
		t.Fatalf("stdin value not forwarded: %q", body["/context/k"])
	}
}

func TestLsWatchStreamsSnapshot(t *testing.T) {
	// The SSE endpoint pushes one snapshot then closes; watchSessions renders it
	// and exits cleanly on the stream end (the JSON path avoids terminal escapes).
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events/stream" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("stub server must support flushing for SSE")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"sessions\":[{\"id\":\"W-1\",\"status\":\"working\"}]}\n\n"))
		fl.Flush()
		// Returning closes the stream, which watchSessions treats as a clean end.
	})
	out, err := runCLI(t, addr, "ls", "--watch", "--json")
	if err != nil {
		t.Fatalf("ls --watch: %v", err)
	}
	if !strings.Contains(out, "W-1") {
		t.Fatalf("watch did not render the snapshot: %q", out)
	}
}

func TestDoctorCmdRendersReport(t *testing.T) {
	// A stub daemon answers /healthz so the daemon check passes; the binary checks
	// reflect the host, so we assert the report renders (not the overall verdict,
	// which depends on whether tmux/gh/claude happen to be installed here).
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	out, _ := runCLI(t, addr, "doctor")
	for _, want := range []string{"warden doctor", "daemon", "reachable at", "data dir"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor report missing %q:\n%s", want, out)
		}
	}
}

func TestStatusCmdJSON(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /sessions/A-1": `{"id":"A-1","name":"alpha","status":"working","type":"development"}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "status", "A-1", "--json")
	if err != nil {
		t.Fatalf("status --json: %v", err)
	}
	if !strings.Contains(out, `"id": "A-1"`) {
		t.Fatalf("status --json output: %q", out)
	}
}

func TestLsTagFilterAgainstDaemon(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /sessions": `{"sessions":[{"id":"A-1","tags":["backend"]},{"id":"B-2","tags":["frontend"]}]}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "ls", "--tag", "backend")
	if err != nil {
		t.Fatalf("ls --tag: %v", err)
	}
	if !strings.Contains(out, "A-1") || strings.Contains(out, "B-2") {
		t.Fatalf("--tag must keep only matching agents: %q", out)
	}
}

func TestMsgSendCmdAsAgent(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "agent-self")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /sessions/B-2/messages": `{"message":{"id":"3","from":"agent-self","to":"B-2","body":"hi"},"woke":false}`,
	}, nil, body))
	out, err := runCLI(t, addr, "msg", "send", "B-2", "hi")
	if err != nil {
		t.Fatalf("msg send: %v", err)
	}
	if !strings.Contains(out, "sent to B-2") || strings.Contains(out, "woke recipient") {
		t.Fatalf("msg send output: %q", out)
	}
	if !strings.Contains(body["/sessions/B-2/messages"], `"from":"agent-self"`) {
		t.Fatalf("WARDEN_SESSION_ID must be the sender: %q", body["/sessions/B-2/messages"])
	}
}

func TestMsgWaitCmdDelivers(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /sessions/A-1/messages/wait": `{"found":true,"message":{"id":"9","from":"B-2","body":"the reply","read":false}}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "msg", "wait", "--as", "A-1", "--from", "B-2", "--timeout", "1")
	if err != nil {
		t.Fatalf("msg wait: %v", err)
	}
	if !strings.Contains(out, "from B-2") || !strings.Contains(out, "the reply") {
		t.Fatalf("msg wait delivered output: %q", out)
	}
}
