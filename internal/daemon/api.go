package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/collab"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/store"
)

// EventRequest is the body for POST /events (sent by hooks).
type EventRequest struct {
	Session string `json:"session"` // tmux session name == session id
	Type    string `json:"type"`
	Detail  string `json:"detail"`
}

// SpawnRequest is the body for POST /spawn.
type SpawnRequest struct {
	Type           string   `json:"type"`            // typed mode: task type (normalized); empty = free-form
	Ticket         string   `json:"ticket"`          // optional; becomes the id when present
	Name           string   `json:"name"`            // optional; human-readable name for the agent
	Repo           string   `json:"repo"`            // required in typed mode
	Branch         string   `json:"branch"`          // optional; development branch / pr-review checkout
	PR             string   `json:"pr"`              // optional; pr-review
	Worktree       bool     `json:"worktree"`        // analysis/spike opt-in
	InRepo         bool     `json:"in_repo"`         // write-agent opt-out: share the repo instead of isolating (ignored for pr-review)
	Prompt         string   `json:"prompt"`          // free-form: the agent's initial prompt; empty = interactive
	Cwd            string   `json:"cwd"`             // free-form: dir to launch claude from (caller cwd / web pick)
	PermissionMode string   `json:"permission_mode"` // explicit permission mode; empty = use global default
	AutoRestart    bool     `json:"auto_restart"`    // opt-in: auto-resume on error (capped)
	Force          bool     `json:"force"`           // bypass the memory-pressure spawn gate
	Model          string   `json:"model"`           // claude model (opus/sonnet/haiku or full ID); empty = default
	Tags           []string `json:"tags"`            // optional free-form labels for grouping/filtering (#30)
}

// confirmationResponse is the 428 body when the spawn gate warns. The client
// turns confirmation_required into ErrConfirmationRequired so surfaces can
// re-spawn with force=true.
type confirmationResponse struct {
	ConfirmationRequired bool             `json:"confirmation_required"`
	Verdict              pressure.Verdict `json:"verdict"`
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
	// DeleteAdoptedBranch overrides the BranchCreated gate so an adopted
	// (human-created) branch is git branch -D'd too. Off by default.
	DeleteAdoptedBranch bool `json:"delete_adopted_branch"`
}

// pruneRequest is the body for POST /prune.
type pruneRequest struct {
	Repo            string `json:"repo"`
	DryRun          bool   `json:"dry_run"`
	Force           bool   `json:"force"`
	IncludeArchived bool   `json:"include_archived"`
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
	// approvals gates the approvals-inbox endpoints (approvals config setting).
	approvals bool
	// cstore is the shared-context KV store (the inter-agent blackboard).
	cstore *ctxstore.Store
	// mbox is the directed-message inbox store.
	mbox *mailbox.Store
	// exec drives pipeline execution (nil if pipelines are unused).
	exec *Executor
	// collab scans active worktrees for inter-agent file conflicts.
	collab *collab.Monitor
	// collabInterval is the file-conflict poll interval; <=0 disables the monitor.
	collabInterval time.Duration
	// narrator produces the digest's LLM summary (nil ⇒ degrade to LastMessage).
	narrator digest.Narrator
	// pressure caching for the spawn gate + GET /pressure. Sampled by a
	// background loop (sibling to the poller); read on the spawn hot path.
	pressMu      sync.RWMutex
	pressLevel   pressure.Level
	spawnGate    bool // spawn_gate config setting
	spawnGateMax int  // spawn_gate_max_agents config setting
	// metrics collection (resource observability). nil collector ⇒ /metrics
	// returns an empty sample; nil recorder ⇒ no on-disk recording.
	mcollector *metrics.Collector
	mrecorder  *metrics.Recorder
	metricsOn  bool // metrics config setting — gates the disk recorder goroutine
	// Context-token bands reused by the metrics-history anomaly detector so its
	// "context climbing/critical" warnings line up with the poller's guard. 0 ⇒
	// the corresponding check is disabled.
	mTokenWarn int
	mTokenCrit int
	// Worktree retention policy (worktree_keep_done / worktree_auto_prune).
	// Both default to false (zero value) = today's keep-everything behavior, so a
	// bare Server literal is non-breaking. removeDoneWorktree=true (config
	// worktree_keep_done=false) removes a clean worktree when its owner is
	// archived; autoPruneWorktree=true runs an unattended orphan sweep.
	removeDoneWorktree bool
	autoPruneWorktree  bool
	// authToken is the bearer token required on every request. Empty ⇒ auth is
	// disabled (the default loopback-only mode). See authorize.
	authToken string
	// authLimiter throttles repeated failed-auth attempts per source IP; nil
	// when auth is disabled. See authlimit.go.
	authLimiter *authLimiter
	// audit is the append-only action trail (audit.jsonl). nil ⇒ auditing off;
	// recordAudit is then a no-op. See audit_hook.go.
	audit *audit.Writer
	// snap captures/lists/restores worktree+transcript snapshots (#46). nil ⇒ the
	// feature is unconfigured; snapshots additionally gates the endpoints (config
	// `snapshots`). See snapshot_routes.go.
	snap      *snapshot.Manager
	snapshots bool
	// plugins dispatches lifecycle hook events to registered plugin executables
	// (#47). nil ⇒ the plugin system is off (the default); Dispatch is then a
	// no-op. Dispatch is always fail-open, so it never alters request flow.
	plugins *plugin.Dispatcher
}

// SetAuth configures the bearer token required for remote access. An empty
// token disables authentication (the local-only default). When set, every
// request — including loopback — must present the token; see authorize. A
// per-IP auth-failure throttle is enabled alongside the token.
func (s *Server) SetAuth(token string) {
	s.authToken = token
	if token != "" {
		s.authLimiter = newAuthLimiter(authFailMax, authFailWindow)
	}
}

// SetWorktreeRetention wires the worktree retention policy. keepDone mirrors
// worktree_keep_done (true = leave archived worktrees in place, today's
// behavior); autoPrune mirrors worktree_auto_prune (true = run the unattended
// orphan sweep). The unattended sweep NEVER touches archived-owned worktrees —
// that reclaim is manual (`warden prune --include-archived`) only.
func (s *Server) SetWorktreeRetention(keepDone, autoPrune bool) {
	s.removeDoneWorktree = !keepDone
	s.autoPruneWorktree = autoPrune
}

// notify signals SSE subscribers that session state changed. Safe with a nil
// hub (some tests construct Server literals without one).
func (s *Server) notify() {
	if s.hub != nil {
		s.hub.publish()
	}
}

// Notify is the exported SSE-notify hook the Executor calls after it changes state.
func (s *Server) Notify() { s.notify() }

// SetExecutor wires the executor after construction (executor needs Server.Notify).
func (s *Server) SetExecutor(e *Executor) { s.exec = e }

// SetNarrator wires the digest narrator after construction (optional; nil ⇒ the
// digest summary degrades to the agent's last transcript message).
func (s *Server) SetNarrator(n digest.Narrator) { s.narrator = n }

// SetSpawnGate configures the memory-pressure spawn gate. enabled=false leaves
// the gauge live but never warns on spawn. Initializes the cached level to
// Normal so reads before the first sample are safe.
func (s *Server) SetSpawnGate(enabled bool, maxAgents int) {
	s.pressMu.Lock()
	defer s.pressMu.Unlock()
	s.spawnGate = enabled
	s.spawnGateMax = maxAgents
	if s.pressLevel == 0 {
		s.pressLevel = pressure.Normal
	}
}

// Lifecycle is the subset of operations the API delegates to (Phase 4+).
// The daemon defines this interface in terms of its own SpawnRequest DTO so
// Phase 2 stays decoupled from the lifecycle package (built in Phase 4). The
// Phase 4 adapter translates daemon.SpawnRequest → lifecycle.SpawnRequest.
type Lifecycle interface {
	Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error)
	Classify(ctx context.Context, prompt string) (store.Type, error)
	// GenerateName derives a short human-friendly handle from a task prompt (local
	// LLM when available, else a deterministic slug). "" means no usable name.
	GenerateName(ctx context.Context, prompt string) string
	// Terminate kills the agent's tmux session (keeps record + worktree).
	Terminate(ctx context.Context, tmuxSession string) error
	// RemoveWorktree removes the session's git worktree + branch (explicit).
	RemoveWorktree(ctx context.Context, sess *store.Session, force, deleteAdoptedBranch bool) error
	// ListWorktrees is the read-only join behind `warden worktree ls`: git
	// worktrees under repo/.worktrees labelled by their owning record + state.
	ListWorktrees(ctx context.Context, repo string, active, archived []*store.Session) ([]lifecycle.WorktreeListing, error)
	// PruneWorktrees reconciles git's worktree list against the supplied records
	// and reclaims orphans under the dirty/unpushed guard.
	PruneWorktrees(ctx context.Context, repo string, opts lifecycle.PruneOpts) ([]lifecycle.PruneResult, error)
	// Teardown force-removes a session's tmux session (and worktree/branch, if
	// any) using the already-known doc, without consulting the store. It is used
	// to roll back Spawn's side effects when persisting the doc fails.
	Teardown(ctx context.Context, sess *store.Session) error
	// Restore recreates and resumes a lost session from its stored doc.
	Restore(ctx context.Context, sess *store.Session) error
	// NewestClaudeSession returns the claude session id of the newest transcript
	// for cwd (ErrNoTranscript when none).
	NewestClaudeSession(ctx context.Context, cwd string) (string, error)
	// Adopt registers a session warden did not spawn (resume or live) and
	// returns the unpersisted record.
	Adopt(ctx context.Context, req AdoptParams) (*store.Session, error)
	Input(ctx context.Context, tmuxSession, text string) error
	Output(ctx context.Context, tmuxSession string, lines int) (string, error)
	// SendKeys injects a raw keystroke (e.g. a menu digit) into the agent's pane.
	SendKeys(ctx context.Context, tmuxSession, key string) error
	// SpawnJob launches one pipeline-job agent (worktree strategy + pipeline env).
	SpawnJob(ctx context.Context, req lifecycle.JobSpawnRequest) (*store.Session, error)
	// CommitWorktree stages+commits any changes in dir; committed=false when clean.
	// Used on job emit so a job's work lands on its branch before downstream forks.
	CommitWorktree(ctx context.Context, dir, message string) (bool, error)
	// Commit / Push / Sync back the wd commit/push/sync CLI + MCP tools: the
	// rail-enforcing git lifecycle returning compact structs instead of git
	// tool-spam Claude reads.
	Commit(ctx context.Context, dir, message string) (lifecycle.CommitResult, error)
	Push(ctx context.Context, dir string) (lifecycle.PushResult, error)
	Sync(ctx context.Context, dir, base string) (lifecycle.SyncResult, error)
	// CreatePR opens a GitHub PR for dir's branch (backs `done --create-pr`).
	CreatePR(ctx context.Context, dir, title, body, base string) (lifecycle.PRResult, error)
	// Check runs the project's configured .warden/check.yml command(s) in dir and
	// returns a pass/fail summary with output only for failures — backs wd check /
	// mcp__warden__check.
	Check(ctx context.Context, dir, name string) (lifecycle.CheckResult, error)
	// TranscriptPath resolves the agent's transcript file ("" when none).
	TranscriptPath(sess *store.Session) string
	// GitBranch / GitNumstat read git state in dir (best-effort, "" on error).
	GitBranch(ctx context.Context, dir string) string
	GitNumstat(ctx context.Context, dir string) string
	// MemoryPressure reads the current macOS memory-pressure level (Normal on
	// non-macOS / error). Used by the sampler loop and the spawn gate.
	MemoryPressure(ctx context.Context) (pressure.Level, error)
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
			slog.Error("daemon: recovered panic in handler", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
			writeErr(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) router() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverMiddleware)
	r.Use(maxBytes(maxBodyBytes))
	r.Use(writeTimeout(writeTimeoutDur))

	// Unauthenticated: a liveness probe (for tunnels/proxies) and the static UI.
	// The compiled SPA holds no secrets — serving its shell to anyone is what
	// lets a remote browser load the app and prompt for a token. All data- and
	// action-bearing routes live in the authenticated group below.
	r.Get("/healthz", s.handleHealthz)

	r.Group(func(ar chi.Router) {
		ar.Use(s.authMiddleware) // bearer-token gate for the API, SSE, and WS
		ar.Get("/sessions", s.handleListSessions)
		ar.Get("/sessions/{id}", s.handleGetSession)
		ar.Post("/events", s.handleEvent)
		ar.Post("/hooks/guard", s.handleGuard)
		ar.Get("/events/stream", s.handleEventsStream)
		// Lifecycle routes: POST /spawn, /sessions/{id}/{terminate,delete,
		// remove-worktree,input,restore}, GET /sessions/{id}/{output,attach}.
		s.registerLifecycleRoutes(ar)
		ar.Get("/fs/dirs", s.handleListDirs)
		ar.Get("/approvals", s.handleApprovals)
		ar.Post("/sessions/{id}/approve", s.handleApprove)
		ar.Patch("/sessions/{id}/auto-approve", s.handleSetAutoApprove)
		ar.Patch("/sessions/{id}/permission-mode", s.handleSetPermissionMode)
		ar.Patch("/sessions/{id}/name", s.handleSetName)
		ar.Get("/sessions/{id}/digest", s.handleDigest)
		ar.Get("/metrics", s.handleMetrics)
		ar.Get("/metrics/history", s.handleMetricsHistory)
		s.registerContextRoutes(ar)
		s.registerMessageRoutes(ar)
		s.registerCollabRoutes(ar)
		s.registerPipelineRoutes(ar)
		s.registerHistoryRoutes(ar)
		s.registerSearchRoutes(ar)
		s.registerImportRoutes(ar)
		s.registerSnapshotRoutes(ar)
	})

	s.registerStatic(r) // unauthenticated catch-all; must be last
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
	sess, err := s.store.GetByNameOrID(r.Context(), id)
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

// reconcileJobOnTerminal reconciles a pipeline job when its session reaches a
// terminal status outside the poller's view — both the SessionEnd hook and the
// terminate handler set "done" directly (no poller swap). The poller skips
// terminal sessions, so without this a job still "running" when its agent ends
// would stay stuck forever. OnTransition's guard leaves an already-completed
// (emit'd) job untouched; a still-running one is failed.
func (s *Server) reconcileJobOnTerminal(sess *store.Session, to store.Status) {
	if s.exec != nil && sess != nil && sess.PipelineID != "" {
		s.exec.OnTransition(sess, sess.Status, to)
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
	to := statusForHook(req.Type)
	// Append the event and apply any status transition in one atomic write so a
	// crash can't log the event without the status change (or vice versa).
	if err := s.store.AppendEventStatus(ctx, req.Session, ev, to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Fail soft: never error a hook for an unknown session.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	// The SessionEnd hook moves a session to a terminal status (done) — reconcile
	// the owning pipeline job (see reconcileJobOnTerminal).
	if to == store.StatusDone {
		if sess, gerr := s.store.Get(ctx, req.Session); gerr == nil {
			s.reconcileJobOnTerminal(sess, to)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
