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

func TestHandleSpawnGateWarns(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, life: &fakeLife{}, spawnGate: true, spawnGateMax: 1, pressLevel: pressure.Normal}
	// One live agent so count(1) >= max(1) → elevated.
	// Use "live1" as the seeded ID; fakeLife.Spawn returns "agent-test" for
	// prompt-mode so there is no collision.
	fs.Insert(context.Background(), &store.Session{ID: "live1", Status: store.StatusWorking})

	body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/spawn", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSpawn(rec, req)

	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428", rec.Code)
	}
	var resp confirmationResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.ConfirmationRequired || !resp.Verdict.Elevated {
		t.Fatalf("expected confirmation_required + elevated verdict, got %+v", resp)
	}
}

func TestHandleSpawnGateForceBypasses(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, life: &fakeLife{}, spawnGate: true, spawnGateMax: 1, pressLevel: pressure.Critical}
	fs.Insert(context.Background(), &store.Session{ID: "live1", Status: store.StatusWorking})

	body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir(), Force: true})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/spawn", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSpawn(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("force spawn status = %d, want 201", rec.Code)
	}
}

func TestHandleSpawnGateDisabledProceeds(t *testing.T) {
	fs := newFakeStore()
	s := &Server{store: fs, life: &fakeLife{}, spawnGate: false, spawnGateMax: 1, pressLevel: pressure.Critical}
	fs.Insert(context.Background(), &store.Session{ID: "live1", Status: store.StatusWorking})

	body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: t.TempDir()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/spawn", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSpawn(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("gate-off status = %d, want 201", rec.Code)
	}
}
