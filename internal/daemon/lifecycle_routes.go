package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/store"
)

func (s *Server) registerLifecycleRoutes(r chi.Router) {
	r.Post("/spawn", s.handleSpawn)
	r.Post("/cleanup", s.handleCleanup)
	r.Post("/sessions/{id}/input", s.handleInput)
	r.Get("/sessions/{id}/output", s.handleOutput)
}

func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Type == "" || req.Repo == "" {
		writeErr(w, http.StatusBadRequest, "type and repo are required")
		return
	}
	// Reject duplicate spawn on an existing ticket (design §8). No-ticket
	// sessions get a random id, so there is nothing to collide on.
	if req.Ticket != "" {
		if _, err := s.store.Get(r.Context(), req.Ticket); err == nil {
			writeErr(w, http.StatusConflict, "session already exists — use `agentctl attach "+req.Ticket+"`")
			return
		}
	}
	sess, err := s.life.Spawn(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.Insert(r.Context(), sess); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	var req CleanupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := s.life.Cleanup(r.Context(), req.ID, req.Force, req.Hard); err != nil {
		// Guard failures are a client error (409), not a server fault.
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if req.Hard {
		_ = s.store.Delete(r.Context(), req.ID)
	} else {
		_ = s.store.Archive(r.Context(), req.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleaned"})
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	var req InputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.life.Input(r.Context(), sess.TmuxSession, req.Text); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	lines := 200
	if q := r.URL.Query().Get("lines"); q != "" {
		if n, err := strconv.Atoi(q); err == nil {
			lines = n
		}
	}
	out, err := s.life.Output(r.Context(), sess.TmuxSession, lines)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, OutputResponse{Output: out})
}
