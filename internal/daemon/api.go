package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/poller"
	"github.com/srajanpathak/agentctl/internal/store"
)

// EventRequest is the body for POST /events (sent by hooks).
type EventRequest struct {
	Session string `json:"session"` // tmux session name == session id
	Type    string `json:"type"`
	Detail  string `json:"detail"`
}

// SpawnRequest is the body for POST /spawn.
type SpawnRequest struct {
	Type     string `json:"type"`     // required; normalized via store.NormalizeType
	Ticket   string `json:"ticket"`   // optional; becomes the id when present
	Repo     string `json:"repo"`     // required
	Branch   string `json:"branch"`   // optional; development branch / pr-review checkout
	PR       string `json:"pr"`       // optional; pr-review
	Worktree bool   `json:"worktree"` // analysis/spike opt-in
}

// CleanupRequest is the body for POST /cleanup.
type CleanupRequest struct {
	ID    string `json:"id"`
	Force bool   `json:"force"`
	Hard  bool   `json:"hard"`
}

// InputRequest is the body for POST /sessions/{id}/input.
type InputRequest struct {
	Text string `json:"text"`
}

// OutputResponse is the body for GET /sessions/{id}/output.
type OutputResponse struct {
	Output string `json:"output"`
}

// errorResponse is the standard error envelope.
type errorResponse struct {
	Error string `json:"error"`
}

// sessionsResponse wraps a list for GET /sessions.
type sessionsResponse struct {
	Sessions []*store.Session `json:"sessions"`
}

// Server holds the daemon's dependencies. store is the single writer.
type Server struct {
	store        store.Store
	life         Lifecycle
	poller       *poller.Poller
	pollInterval time.Duration
	hub          *hub
}

// notify signals SSE subscribers that session state changed. Safe with a nil
// hub (some tests construct Server literals without one).
func (s *Server) notify() {
	if s.hub != nil {
		s.hub.publish()
	}
}

// Lifecycle is the subset of operations the API delegates to (Phase 4+).
// The daemon defines this interface in terms of its own SpawnRequest DTO so
// Phase 2 stays decoupled from the lifecycle package (built in Phase 4). The
// Phase 4 adapter translates daemon.SpawnRequest → lifecycle.SpawnRequest.
type Lifecycle interface {
	Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error)
	Cleanup(ctx context.Context, id string, force, hard bool) error
	Input(ctx context.Context, tmuxSession, text string) error
	Output(ctx context.Context, tmuxSession string, lines int) (string, error)
}

func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealthz)
	r.Get("/sessions", s.handleListSessions)
	r.Get("/sessions/{id}", s.handleGetSession)
	r.Post("/events", s.handleEvent)
	r.Get("/events/stream", s.handleEventsStream)
	// Phase 4 / 7 register: POST /spawn, POST /cleanup,
	// POST /sessions/{id}/input, GET /sessions/{id}/output.
	s.registerLifecycleRoutes(r)
	s.registerStatic(r) // catch-all; must be last
	return r
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResponse{Error: msg})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "mongo unavailable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []*store.Session{}
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: sessions})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// statusForHook maps a Claude hook event type to a status (design §6 table).
// An empty status means "log the event but do not change status".
func statusForHook(t string) store.Status {
	switch t {
	case "SessionStart":
		return store.StatusWorking
	case "Notification":
		return store.StatusWaitingForInput
	case "Stop":
		return store.StatusIdle
	default: // SubagentStop and others: event-log only
		return ""
	}
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	var req EventRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	ctx := r.Context()
	ev := store.Event{Type: req.Type, Detail: req.Detail}
	if err := s.store.AppendEvent(ctx, req.Session, ev); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Fail soft: never error a hook for an unknown session.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st := statusForHook(req.Type); st != "" {
		if err := s.store.UpdateStatus(ctx, req.Session, st); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
