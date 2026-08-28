package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestStartFreeFormPrompt drives the autonomous free-form spawn path (a prompt,
// no --type) against a stub daemon and asserts the prompt is forwarded and the
// "classifying" outcome is rendered.
func TestStartFreeFormPrompt(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"code-1","name":"scout","status":"spawning"}`,
	}, nil, body))
	out, err := runCLI(t, addr, "start", "research SSE reconnection", "--name", "scout", "--role", "general")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(out, "spawned code-1 (scout) (classifying…)") {
		t.Fatalf("start output: %q", out)
	}
	if !strings.Contains(body["/api/v1/spawn"], `"prompt":"research SSE reconnection"`) {
		t.Fatalf("prompt not forwarded: %q", body["/api/v1/spawn"])
	}
}

// TestStartInteractiveNoPrompt covers the interactive variant (no prompt, no
// --type): the daemon still spawns, but the CLI reports an interactive agent.
func TestStartInteractiveNoPrompt(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"code-2","status":"spawning"}`,
	}, nil, nil))
	out, err := runCLI(t, addr, "start", "--dir", t.TempDir(), "--role", "general")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(out, "opened interactive agent code-2") {
		t.Fatalf("start interactive output: %q", out)
	}
}

// TestStartTypedManaged covers the typed/managed-worktree branch.
func TestStartTypedManaged(t *testing.T) {
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"DEV-1","type":"development","status":"spawning"}`,
	}, nil, body))
	out, err := runCLI(t, addr, "start", "DEV-1", "--type", "development", "--repo", t.TempDir(), "--tags", "backend, urgent", "--role", "worker")
	if err != nil {
		t.Fatalf("start typed: %v", err)
	}
	if !strings.Contains(out, "spawned DEV-1 [development] (spawning)") {
		t.Fatalf("start typed output: %q", out)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body["/api/v1/spawn"]), &sent); err != nil {
		t.Fatalf("spawn body not JSON: %v", err)
	}
	if sent["type"] != "development" || sent["ticket"] != "DEV-1" {
		t.Fatalf("typed spawn fields: %v", sent)
	}
	tags, _ := sent["tags"].([]any)
	if len(tags) != 2 || tags[0] != "backend" || tags[1] != "urgent" {
		t.Fatalf("tags not parsed/forwarded: %v", sent["tags"])
	}
}

// TestStartPRReviewNeedsTarget asserts the pr-review guard fires before any spawn.
func TestStartPRReviewNeedsTarget(t *testing.T) {
	called := false
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	if _, err := runCLI(t, addr, "start", "REV-1", "--type", "pr-review", "--repo", t.TempDir()); err == nil {
		t.Fatal("pr-review without --pr/--branch must error")
	}
	if called {
		t.Fatal("the guard must fire before contacting the daemon")
	}
}

// TestStartMemoryPressureGate covers the 428 confirmation-required path: the CLI
// surfaces a friendly gate message and a non-nil error (so it's re-runnable with
// --force) instead of spawning.
func TestStartMemoryPressureGate(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"confirmation_required": true,
			"verdict":               map[string]any{"elevated": true, "reason": "pressure: warn"},
		})
	})
	out, err := runCLI(t, addr, "start", "do a thing", "--role", "general")
	if err == nil {
		t.Fatal("a warned memory-pressure gate must return an error")
	}
	if !strings.Contains(out, "memory pressure: pressure: warn") {
		t.Fatalf("expected the gate warning on stderr: %q", out)
	}
}

func TestPluginListCmd(t *testing.T) {
	// Default config: the plugin system is off and nothing is registered.
	out, err := runCLI(t, "", "plugin", "list")
	if err != nil {
		t.Fatalf("plugin list: %v", err)
	}
	if !strings.Contains(out, "plugin system: disabled") || !strings.Contains(out, "no plugins registered") {
		t.Fatalf("plugin list output: %q", out)
	}
}
