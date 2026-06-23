//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// TestSpawnTerminateCleanup exercises the full agent lifecycle against a real
// tmux + claude: spawn → appears in the fleet → terminate → delete → gone.
// It is skipped unless both binaries are present, so it runs locally (where a
// developer has claude installed) but does not break CI, which has neither.
//
// It asserts only lifecycle transitions, not agent output — what the agent
// "says" depends on a live model and is not a property this suite can pin.
func TestSpawnTerminateCleanup(t *testing.T) {
	if !hasBinary("tmux") {
		t.Skip("tmux not installed; skipping spawn lifecycle test")
	}
	if !hasBinary("claude") {
		t.Skip("claude not installed; skipping spawn lifecycle test")
	}

	h := startDaemon(t)
	workdir := t.TempDir()

	// Spawn a prompt-mode agent in an isolated workdir (no worktree needed).
	h.mustWd("start", "print hello and then stop", "--dir", workdir, "--type", "other")

	id := waitForOneSession(t, h, 15*time.Second)

	// Tear it down: terminate kills the tmux+claude session but keeps the
	// record; delete --hard then purges it.
	h.mustWd("terminate", id)
	h.mustWd("delete", id, "--hard")

	if n := sessionCount(t, h); n != 0 {
		t.Fatalf("expected fleet empty after delete, got %d sessions", n)
	}
}

// waitForOneSession polls `ls --json` until exactly one session exists and
// returns its id, or fails on timeout.
func waitForOneSession(t *testing.T, h *harness, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out := h.mustWd("ls", "--json")
		var sessions []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &sessions); err != nil {
			t.Fatalf("ls --json invalid: %v\n%s", err, out)
		}
		if len(sessions) == 1 {
			return sessions[0].ID
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("spawned agent never appeared in the fleet")
	return ""
}

// sessionCount returns the number of active sessions reported by `ls --json`.
func sessionCount(t *testing.T, h *harness) int {
	t.Helper()
	out := h.mustWd("ls", "--json")
	var sessions []json.RawMessage
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("ls --json invalid: %v\n%s", err, out)
	}
	return len(sessions)
}
