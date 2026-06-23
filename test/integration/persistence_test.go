//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSessionSurvivesRestart seeds a session record on disk, then verifies the
// daemon loads it from the file store on startup and still serves it after a
// full stop/start cycle — the recovery path that keeps a fleet visible across
// daemon restarts (and machine reboots).
func TestSessionSurvivesRestart(t *testing.T) {
	// Bring the daemon up once so it creates the data-dir layout, then take it
	// down so we can seed a record without racing the single-writer store.
	h := startDaemon(t)
	h.stop()

	const id = "itest-recovered"
	seedSession(t, h.dataDir(), id)

	h.launch()
	assertListed(t, h, id)

	h.restart()
	assertListed(t, h, id)
}

// seedSession writes a minimal but valid session JSON into the active store.
func seedSession(t *testing.T, dataDir, id string) {
	t.Helper()
	now := time.Now().UTC()
	rec := map[string]any{
		"id":           id,
		"type":         "development",
		"tmux_session": id,
		"repo":         "/repo",
		"status":       "idle",
		"created_at":   now.Format(time.RFC3339Nano),
		"updated_at":   now.Format(time.RFC3339Nano),
		"events":       []any{},
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed session: %v", err)
	}
	dir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
		t.Fatalf("write seed session: %v", err)
	}
}

// assertListed fails unless `ls --json` reports a session with the given id.
func assertListed(t *testing.T, h *harness, id string) {
	t.Helper()
	out := h.mustWd("ls", "--json")
	var sessions []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("ls --json invalid: %v\n%s", err, out)
	}
	for _, s := range sessions {
		if s.ID == id {
			return
		}
	}
	t.Fatalf("session %q not listed after restart:\n%s", id, out)
}
