package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeResumeFile drops a non-empty handoff notes file and returns its path.
func writeResumeFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "handoff.md")
	if err := os.WriteFile(p, []byte("decisions and next steps"), 0o644); err != nil {
		t.Fatalf("write resume file: %v", err)
	}
	return p
}

func TestHandoffToExistingAgent(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /sessions/B-2":           `{"id":"B-2","status":"working"}`,
		"POST /sessions/B-2/messages": `{"message":{"id":"7","from":"human","to":"B-2","body":"x"},"woke":true}`,
	}, nil, body))
	rf := writeResumeFile(t)
	out, err := runCLI(t, addr, "handoff", "--to", "B-2", "--resume-file", rf, "--resume-prompt", "finish the migration")
	if err != nil {
		t.Fatalf("handoff --to: %v", err)
	}
	if !strings.Contains(out, "handed off to B-2") || !strings.Contains(out, "woke recipient") {
		t.Fatalf("handoff --to output: %q", out)
	}
	if !strings.Contains(body["/sessions/B-2/messages"], "finish the migration") {
		t.Fatalf("resume prompt not delivered in the message body: %q", body["/sessions/B-2/messages"])
	}
}

func TestHandoffToMissingTargetFailsFast(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	sent := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			sent = true
		}
		// The target lookup 404s.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such session"}`))
	})
	rf := writeResumeFile(t)
	if _, err := runCLI(t, addr, "handoff", "--to", "ghost", "--resume-file", rf, "--resume-prompt", "go"); err == nil {
		t.Fatal("a missing target must error")
	}
	if sent {
		t.Fatal("must verify the target exists before sending the handoff")
	}
}

func TestHandoffNewDelegate(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /spawn": `{"id":"DEL-1","type":"development","status":"spawning"}`,
	}, nil, nil))
	rf := writeResumeFile(t)
	out, err := runCLI(t, addr, "handoff", "--repo", t.TempDir(), "--type", "development",
		"--resume-file", rf, "--resume-prompt", "build the thing")
	if err != nil {
		t.Fatalf("handoff new: %v", err)
	}
	if !strings.Contains(out, "delegated to fresh agent DEL-1") {
		t.Fatalf("handoff new output: %q", out)
	}
}

func TestHandoffRequiresResumeFlags(t *testing.T) {
	if _, err := runCLI(t, "", "handoff"); err == nil {
		t.Fatal("handoff without --resume-file/--resume-prompt must error")
	}
}
