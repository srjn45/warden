package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/mailbox"
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
	Worktree   bool   `json:"worktree"`   // analysis/spike opt-in
	Prompt     string `json:"prompt"`     // prompt-mode: the agent's initial prompt
	Cwd        string `json:"cwd"`        // dir to launch claude from (caller cwd / web pick)
	Supervised bool   `json:"supervised"` // opt-in supervised mode (acceptEdits prompts)
}

// AdoptRequest is the body for POST /adopt.
type AdoptRequest struct {
	Cwd         string `json:"cwd"`          // required; dir whose claude session to adopt
	SessionID   string `json:"session_id"`   // optional claude uuid override
	TmuxSession string `json:"tmux_session"` // non-empty ⇒ live-register an existing tmux session
}

// AdoptParams are the resolved inputs the handler passes to Lifecycle.Adopt.
type AdoptParams struct {
	ID              string // chosen id; "" ⇒ Lifecycle generates one
	Cwd             string
	ClaudeSessionID string // may be "" in live mode
	TmuxSession     string // "" ⇒ resume mode
}

// adoptResponse is the body for POST /adopt: the new session plus an optional
// non-fatal warning (e.g. live-registered without a resolvable claude id).
type adoptResponse struct {
	Session *store.Session `json:"session"`
	Warning string         `json:"warning,omitempty"`
}

type deleteRequest struct {
	Hard bool `json:"hard"`
}
type removeWorktreeRequest struct {
	Force bool `json:"force"`
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
	// done is closed when the server begins shutting down. Long-lived handlers
	// (the SSE stream) watch it so they return promptly and let Shutdown drain.
	done chan struct{}
	// approvals gates the approvals-inbox endpoints (AGENTCTL_APPROVALS).
	approvals bool
	// cstore is the shared-context KV store (the inter-agent blackboard).
	cstore *ctxstore.Store
	// mbox is the directed-message inbox store.
	mbox *mailbox.Store
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
	Classify(ctx context.Context, prompt string) (store.Type, error)
	// Terminate kills the agent's tmux session (keeps record + worktree).
	Terminate(ctx context.Context, tmuxSession string) error
	// RemoveWorktree removes the session's git worktree + branch (explicit).
	RemoveWorktree(ctx context.Context, sess *store.Session, force bool) error
	// Teardown force-removes a session's tmux session (and worktree/branch, if
	// any) using the already-known doc, without consulting the store. It is used
	// to roll back Spawn's side effects when persisting the doc fails.
	Teardown(ctx context.Context, sess *store.Session) error
	// Restore recreates and resumes a lost session from its stored doc.
	Restore(ctx context.Context, sess *store.Session) error
	// NewestClaudeSession returns the claude session id of the newest transcript
	// for cwd (ErrNoTranscript when none).
	NewestClaudeSession(ctx context.Context, cwd string) (string, error)
	// Adopt registers a session agentctl did not spawn (resume or live) and
	// returns the unpersisted record.
	Adopt(ctx context.Context, req AdoptParams) (*store.Session, error)
	Input(ctx context.Context, tmuxSession, text string) error
	Output(ctx context.Context, tmuxSession string, lines int) (string, error)
	// SendKeys injects a raw keystroke (e.g. a menu digit) into the agent's pane.
	SendKeys(ctx context.Context, tmuxSession, key string) error
}

// recoverMiddleware converts a panic in any handler into a 500 response instead
// of a dropped connection, keeping the long-running daemon alive and its log
// readable. http.ErrAbortHandler is intentional and re-panicked so net/http can
// handle it.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			log.Printf("daemon: recovered panic in %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			writeErr(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverMiddleware)
	r.Get("/healthz", s.handleHealthz)
	r.Get("/sessions", s.handleListSessions)
	r.Get("/sessions/{id}", s.handleGetSession)
	r.Post("/events", s.handleEvent)
	r.Get("/events/stream", s.handleEventsStream)
	// Lifecycle routes: POST /spawn, /sessions/{id}/{terminate,delete,
	// remove-worktree,input,restore}, GET /sessions/{id}/{output,attach}.
	s.registerLifecycleRoutes(r)
	r.Get("/fs/dirs", s.handleListDirs)
	r.Get("/approvals", s.handleApprovals)
	r.Post("/sessions/{id}/approve", s.handleApprove)
	s.registerContextRoutes(r)
	s.registerMessageRoutes(r)
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
		writeErr(w, http.StatusServiceUnavailable, "store unavailable: "+err.Error())
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
	case "SessionEnd":
		// The CLI session ended (claude exited) — terminal. The poller's
		// isTerminal check then leaves it alone, so it won't flip to orphaned
		// when the tmux session later goes away.
		return store.StatusDone
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
	// Append the event and apply any status transition in one atomic write so a
	// crash can't log the event without the status change (or vice versa).
	if err := s.store.AppendEventStatus(ctx, req.Session, ev, statusForHook(req.Type)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Fail soft: never error a hook for an unknown session.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
