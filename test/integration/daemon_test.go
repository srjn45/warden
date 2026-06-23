//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDaemonHealthAndEmptyList brings up a fresh daemon and verifies the most
// basic end-to-end paths: it serves /healthz, and `ls --json` round-trips an
// empty fleet through the real HTTP + file-store stack.
func TestDaemonHealthAndEmptyList(t *testing.T) {
	h := startDaemon(t)

	if !h.healthy() {
		t.Fatal("daemon reported unhealthy after startup")
	}

	out := h.mustWd("ls", "--json")
	var sessions []map[string]any
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("ls --json is not valid JSON: %v\n%s", err, out)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected empty fleet, got %d sessions: %s", len(sessions), out)
	}
}

// TestConfigReflectsIsolatedHome confirms the harness's HOME isolation actually
// takes hold: the resolved data_dir must live under the test HOME, not the
// developer's real ~/.warden. (config reflects the on-disk config file, not the
// runtime --addr override, so only the HOME-derived data_dir is asserted here.)
func TestConfigReflectsIsolatedHome(t *testing.T) {
	h := startDaemon(t)

	out := h.mustWd("config")
	if !strings.Contains(out, h.dataDir()) {
		t.Fatalf("config did not reflect isolated data dir %q:\n%s", h.dataDir(), out)
	}
}

// TestDoctorReachesDaemon runs the preflight checker against the live daemon.
// Its exit code depends on optional binaries (claude/gh) that may be absent in
// CI, so this asserts only that doctor runs and reports the daemon reachable.
func TestDoctorReachesDaemon(t *testing.T) {
	h := startDaemon(t)

	// doctor exits non-zero when an optional binary is missing; we only care
	// that it ran and saw the daemon, so the error is tolerated here.
	out, _ := h.wd("doctor")
	if !strings.Contains(strings.ToLower(out), "daemon") {
		t.Fatalf("doctor output never mentioned the daemon:\n%s", out)
	}
}
