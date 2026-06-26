package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/store"
)

func postSpawn(t *testing.T, s *Server, body SpawnRequest) *http.Response {
	t.Helper()
	ts := httptest.NewServer(s.router())
	t.Cleanup(ts.Close)
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestHandleSpawnGateWarns(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, life: &fakeLife{}, spawnGate: true, spawnGateMax: 1, pressLevel: pressure.Normal}
	// One live agent so count(1) >= max(1) → elevated.
	// Use "live1" as the seeded ID; fakeLife.Spawn returns "agent-test" for
	// prompt-mode so there is no collision.
	fs.Insert(context.Background(), &store.Session{ID: "live1", Status: store.StatusWorking})

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428", resp.StatusCode)
	}
	var out struct {
		ConfirmationRequired bool `json:"confirmation_required"`
		Verdict              struct {
			Elevated bool `json:"elevated"`
		} `json:"verdict"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if !out.ConfirmationRequired || !out.Verdict.Elevated {
		t.Fatalf("expected confirmation_required + elevated verdict, got %+v", out)
	}
}

func TestHandleSpawnGateForceBypasses(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, life: &fakeLife{}, spawnGate: true, spawnGateMax: 1, pressLevel: pressure.Critical}
	fs.Insert(context.Background(), &store.Session{ID: "live1", Status: store.StatusWorking})

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir(), Force: true})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("force spawn status = %d, want 201", resp.StatusCode)
	}
}

func TestHandleSpawnGateDisabledProceeds(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, life: &fakeLife{}, spawnGate: false, spawnGateMax: 1, pressLevel: pressure.Critical}
	fs.Insert(context.Background(), &store.Session{ID: "live1", Status: store.StatusWorking})

	resp := postSpawn(t, s, SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("gate-off status = %d, want 201", resp.StatusCode)
	}
}
