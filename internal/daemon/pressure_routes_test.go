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
	if !got.Elevated {
		t.Error("warn level should report elevated")
	}
	if !got.GateEnabled || got.MaxAgents != 5 {
		t.Errorf("gate flags wrong: enabled=%v max=%d", got.GateEnabled, got.MaxAgents)
	}
}
