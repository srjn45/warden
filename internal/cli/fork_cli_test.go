package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestForkCmdPlumbsForkFrom drives `warden fork <agent> [prompt]` against a stub
// daemon and asserts it is a managed spawn carrying fork_from (+ the trailing prompt
// on the existing prompt seam) with the default worktree-backed type.
func TestForkCmdPlumbsForkFrom(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"development-ab12","type":"development","status":"spawning"}`,
	}, nil, body))
	out, err := runCLI(t, addr, "fork", "src-agent", "now try the other approach")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if !strings.Contains(out, "forked src-agent → development-ab12") {
		t.Fatalf("fork output: %q", out)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body["/api/v1/spawn"]), &sent); err != nil {
		t.Fatalf("spawn body not JSON: %v", err)
	}
	if sent["fork_from"] != "src-agent" {
		t.Fatalf("fork_from not forwarded: %v", sent["fork_from"])
	}
	if sent["type"] != "development" {
		t.Fatalf("fork must default to a worktree-backed type: %v", sent["type"])
	}
	if sent["prompt"] != "now try the other approach" {
		t.Fatalf("trailing prompt not forwarded: %v", sent["prompt"])
	}
}

// TestForkCmdNoPrompt covers the prompt-less form: `warden fork <agent>` just
// continues the source's conversation, sending an empty prompt.
func TestForkCmdNoPrompt(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"development-cd34","type":"development","status":"spawning"}`,
	}, nil, body))
	if _, err := runCLI(t, addr, "fork", "src-agent"); err != nil {
		t.Fatalf("fork: %v", err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(body["/api/v1/spawn"]), &sent)
	if sent["fork_from"] != "src-agent" || sent["prompt"] != "" {
		t.Fatalf("prompt-less fork fields: %v", sent)
	}
}

// TestStartForkFromFlag covers `warden start --fork-from <agent>`: the flag defaults
// the type to development (a fork needs a worktree) and forwards fork_from.
func TestStartForkFromFlag(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"development-ef56","type":"development","status":"spawning"}`,
	}, nil, body))
	if _, err := runCLI(t, addr, "start", "--fork-from", "src-agent", "--repo", t.TempDir(), "--role", "general"); err != nil {
		t.Fatalf("start --fork-from: %v", err)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(body["/api/v1/spawn"]), &sent); err != nil {
		t.Fatalf("spawn body not JSON: %v", err)
	}
	if sent["fork_from"] != "src-agent" {
		t.Fatalf("--fork-from not forwarded: %v", sent["fork_from"])
	}
	if sent["type"] != "development" {
		t.Fatalf("--fork-from must default the type to a worktree-backed one: %v", sent["type"])
	}
}
