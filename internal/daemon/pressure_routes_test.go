package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/pressure"
)

func TestHandlePressure(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, spawnGate: true, spawnGateMax: 5, pressLevel: pressure.Warn}

	req := httptest.NewRequest(http.MethodGet, "/pressure", nil)
	rec := httptest.NewRecorder()
	s.handlePressure(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp pressureResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Level != int(pressure.Warn) || resp.LevelName != "warn" {
		t.Errorf("level = %d/%s, want 2/warn", resp.Level, resp.LevelName)
	}
	if !resp.Elevated {
		t.Error("warn level should report elevated")
	}
	if !resp.GateEnabled || resp.MaxAgents != 5 {
		t.Errorf("gate flags wrong: enabled=%v max=%d", resp.GateEnabled, resp.MaxAgents)
	}
}
