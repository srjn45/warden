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
		"GET /api/v1/sessions/B-2":           `{"id":"B-2","status":"working"}`,
		"POST /api/v1/sessions/B-2/messages": `{"message":{"id":"7","from":"human","to":"B-2","body":"x"},"woke":true}`,
	}, nil, body))
	rf := writeResumeFile(t)
	out, err := runCLI(t, addr, "handoff", "--to", "B-2", "--resume-file", rf, "--resume-prompt", "finish the migration")
	if err != nil {
		t.Fatalf("handoff --to: %v", err)
	}
	if !strings.Contains(out, "handed off to B-2") || !strings.Contains(out, "woke recipient") {
		t.Fatalf("handoff --to output: %q", out)
	}
	if !strings.Contains(body["/api/v1/sessions/B-2/messages"], "finish the migration") {
		t.Fatalf("resume prompt not delivered in the message body: %q", body["/api/v1/sessions/B-2/messages"])
	}
}

func TestHandoffToMissingTargetFailsFast(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	sent := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/v1/messages") {
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
		"POST /api/v1/spawn": `{"id":"DEL-1","type":"development","status":"spawning"}`,
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

// TestHandoffRetireAndToMutuallyExclusive guards the new self-succession mode:
// --retire reaps the caller, --to keeps it running, so combining them is a bug.
func TestHandoffRetireAndToMutuallyExclusive(t *testing.T) {
	rf := writeResumeFile(t)
	_, err := runCLI(t, "", "handoff", "--retire", "--to", "B-2", "--resume-file", rf, "--resume-prompt", "go")
	if err == nil {
		t.Fatal("--retire and --to together must error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected a mutual-exclusion error, got: %v", err)
	}
}

// TestHandoffRetireRequiresConfirm — retire is irreversible (reaps the caller),
// so like `rotate` it refuses to act without --confirm.
func TestHandoffRetireRequiresConfirm(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "SELF-1")
	rf := writeResumeFile(t)
	if _, err := runCLI(t, "", "handoff", "--retire", "--resume-file", rf, "--resume-prompt", "go"); err == nil {
		t.Fatal("handoff --retire without --confirm must error")
	}
}

// TestHandoffRetireEqualsRotate drives the full retire flow against a stub daemon
// and asserts it reproduces `rotate`: a successor is spawned in the SAME worktree
// (cwd) with the inherited permission mode, then the calling agent is reaped via
// terminate — never a worktree removal (that invariant is also a compile-time
// guarantee via the rotator interface).
func TestHandoffRetireEqualsRotate(t *testing.T) {
	t.Setenv("AGENTCTL_SESSION_ID", "")
	t.Setenv("WARDEN_SESSION_ID", "SELF-1")
	seenMethod := map[string]string{}
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/SELF-1": `{"id":"SELF-1","workdir":"/repo/.worktrees/SELF-1","permission_mode":"acceptEdits"}`,
		"POST /api/v1/spawn":          `{"id":"SUCC-1","workdir":"/repo/.worktrees/SELF-1"}`,
	}, seenMethod, body))
	rf := writeResumeFile(t)
	out, err := runCLI(t, addr, "handoff", "--retire", "--confirm",
		"--resume-file", rf, "--resume-prompt", "carry on the migration")
	if err != nil {
		t.Fatalf("handoff --retire --confirm: %v", err)
	}
	if !strings.Contains(out, "rotated: successor SUCC-1 spawned in /repo/.worktrees/SELF-1") {
		t.Fatalf("retire output: %q", out)
	}
	// Successor inherits the caller's worktree (cwd) and permission mode.
	if !strings.Contains(body["/api/v1/spawn"], `"cwd":"/repo/.worktrees/SELF-1"`) ||
		!strings.Contains(body["/api/v1/spawn"], `"permission_mode":"acceptEdits"`) {
		t.Fatalf("successor must reuse the worktree + permission mode: %q", body["/api/v1/spawn"])
	}
	// The successor reads the handoff notes by path (rotate semantics: same worktree).
	if !strings.Contains(body["/api/v1/spawn"], rf) {
		t.Fatalf("successor prompt must point at the handoff file %q: %q", rf, body["/api/v1/spawn"])
	}
	// Retire reaps the caller (terminate), keeping the worktree.
	if seenMethod["/api/v1/sessions/SELF-1/terminate"] != "POST" {
		t.Fatalf("retire must reap the calling agent: %+v", seenMethod)
	}
}

// TestRotateAliasStillWorks — `rotate` is now a thin alias for `handoff --retire`
// and MUST behave identically: same successor-in-place + reap, same output.
func TestRotateAliasStillWorks(t *testing.T) {
	t.Setenv("AGENTCTL_SESSION_ID", "")
	t.Setenv("WARDEN_SESSION_ID", "SELF-1")
	seenMethod := map[string]string{}
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/sessions/SELF-1": `{"id":"SELF-1","workdir":"/repo/.worktrees/SELF-1","permission_mode":"acceptEdits"}`,
		"POST /api/v1/spawn":          `{"id":"SUCC-1","workdir":"/repo/.worktrees/SELF-1"}`,
	}, seenMethod, body))
	rf := writeResumeFile(t)
	out, err := runCLI(t, addr, "rotate", "--confirm", "--resume-file", rf, "--resume-prompt", "carry on")
	if err != nil {
		t.Fatalf("rotate alias: %v", err)
	}
	if !strings.Contains(out, "rotated: successor SUCC-1 spawned in /repo/.worktrees/SELF-1") {
		t.Fatalf("rotate alias output: %q", out)
	}
	if seenMethod["/api/v1/sessions/SELF-1/terminate"] != "POST" {
		t.Fatalf("rotate alias must reap the calling agent: %+v", seenMethod)
	}
}

// TestRotateRequiresConfirm — the alias keeps rotate's irreversibility guard.
func TestRotateRequiresConfirm(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "SELF-1")
	rf := writeResumeFile(t)
	if _, err := runCLI(t, "", "rotate", "--resume-file", rf, "--resume-prompt", "go"); err == nil {
		t.Fatal("rotate without --confirm must error")
	}
}
