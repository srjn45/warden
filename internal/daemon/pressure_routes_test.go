package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/pressure"
)

func TestHandlePressure(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, spawnGate: true, spawnGateMax: 5, pressLevel: pressure.Warn}

	ts := httptest.NewServer(s.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/pressure")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Level       int    `json:"level"`
		LevelName   string `json:"level_name"`
		MaxAgents   int    `json:"max_agents"`
		Elevated    bool   `json:"elevated"`
		GateEnabled bool   `json:"gate_enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Level != int(pressure.Warn) || got.LevelName != "warn" {
		t.Errorf("level = %d/%s, want 2/warn", got.Level, got.LevelName)
	}
	// Warn is advisory, not blocking: the gauge reports the warn level but must
	// NOT flag Elevated (which now means "spawns are blocked"). The UI colours by
	// level_name, so warn is still visibly amber.
	if got.Elevated {
		t.Error("warn level must not report elevated (advisory only)")
	}
	if !got.GateEnabled || got.MaxAgents != 5 {
		t.Errorf("gate flags wrong: enabled=%v max=%d", got.GateEnabled, got.MaxAgents)
	}
}

// TestHandlePressureCriticalElevated confirms the gauge still flags Elevated at
// critical — the level that now hard-blocks spawns.
func TestHandlePressureCriticalElevated(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, spawnGate: true, spawnGateMax: 5, pressLevel: pressure.Critical}

	ts := httptest.NewServer(s.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/pressure")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		LevelName string `json:"level_name"`
		Elevated  bool   `json:"elevated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.LevelName != "critical" || !got.Elevated {
		t.Errorf("critical: level_name=%s elevated=%v, want critical/true", got.LevelName, got.Elevated)
	}
}
