package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/branchtrack"
	"github.com/srjn45/warden/internal/collab"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
)

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
	Backend        string   `json:"backend"`         // agent backend id (claude, aider, …); empty = claude (back-compat)
	Kind           string   `json:"kind"`            // "" / "agent" ⇒ AI agent; "terminal" ⇒ plain ${SHELL:-bash} pane (backend/model/role/prompt ignored)
	Tags           []string `json:"tags"`            // optional free-form labels for grouping/filtering (#30)
	ParentID       string   `json:"parent_id"`       // id of the agent that spawned this one; empty = root (operator/CLI spawn)
	ForkFrom       string   `json:"fork_from"`       // id of an existing agent whose recorded session to FORK (codex fork); empty = normal spawn
	Role           string   `json:"role"`            // built-in role name; empty = general (no persona). Persona injected + role defaults fill unset fields.
	Tier           string   `json:"tier"`            // explicit model tier ("tier-1"/"tier-2"/"tier-3") for the quota-balanced resolver; empty = derive from task/role
	Task           string   `json:"task"`            // task name (task registry) for tier routing via task.TierFor; empty = none
}

// AdoptParams are the resolved inputs the handler passes to Lifecycle.Adopt.
type AdoptParams struct {
	ID              string // chosen id; "" ⇒ Lifecycle generates one
	Cwd             string
	ClaudeSessionID string // may be "" in live mode
	TmuxSession     string // "" ⇒ resume mode
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
	// branchTracker reports each agent's CI status + branch-vs-main state (#44).
	branchTracker *branchtrack.Tracker
	// branchTrackInterval is the branch-tracker poll interval; <=0 disables it.
	branchTrackInterval time.Duration
	// narrator produces the digest's LLM summary (nil ⇒ degrade to LastMessage).
	narrator digest.Narrator
	// pressure caching for the spawn gate + GET /pressure. Sampled by a
	// background loop (sibling to the poller); read on the spawn hot path.
	pressMu      sync.RWMutex
	pressLevel   pressure.Level
	spawnGate    bool // spawn_gate config setting
	spawnGateMax int  // spawn_gate_max_agents config setting
	// Budget (cost) gate: a soft spawn gate on measured Claude spend, sibling to
	// the memory-pressure gate above. Read on the spawn hot path; set once at start.
	budgetGate      bool    // budget_gate config setting
	budgetDailyUSD  float64 // budget_daily_usd cap (0 ⇒ daily axis off)
	budgetWeeklyUSD float64 // budget_weekly_usd cap (0 ⇒ weekly axis off)
	// metrics collection (resource observability). nil collector ⇒ /metrics
	// returns an empty sample; nil recorder ⇒ no on-disk recording.
	mcollector *metrics.Collector
	mrecorder  *metrics.Recorder
	metricsOn  bool // metrics config setting — gates the disk recorder goroutine
	// HTTP write budgets (http_timeout_fast / http_timeout_slow config settings);
	// zero values fall back to writeTimeoutDur / slowWriteTimeoutDur in router().
	writeFast time.Duration
	writeSlow time.Duration
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
	// readonlyToken is an optional second bearer token granting read-only access
	// (scopeReadonly): reads pass, writes and the interactive attach are denied.
	// Empty ⇒ no read-only token. Only meaningful when authToken is also set.
	readonlyToken string
	// authLimiter throttles repeated failed-auth attempts per source IP; nil
	// when auth is disabled. See authlimit.go.
	authLimiter *authLimiter
	// audit is the append-only action trail (audit.jsonl). nil ⇒ auditing off;
	// recordAudit is then a no-op. See audit_hook.go.
	audit *audit.Writer
	// auditTrustedProxies lists proxy/tunnel nets whose X-Forwarded-For the audit
	// actor resolution trusts (see auditActor). nil ⇒ actor is the peer address.
	auditTrustedProxies []*net.IPNet
	// snap captures/lists/restores worktree+transcript snapshots (#46). nil ⇒ the
	// feature is unconfigured; snapshots additionally gates the endpoints (config
	// `snapshots`). See snapshot_routes.go.
	snap      *snapshot.Manager
	snapshots bool
	// savings is the token-savings ledger (the measured token-reduction proof).
	// nil ⇒ unconfigured; savingsOn additionally gates GET /savings (config
	// `savings`). recordCheckSavings is fail-open, so a nil store or a write
	// error never alters the check it measures. See savings_routes.go.
	savings   *savings.Store
	savingsOn bool
	// spend is the per-session real-spend tracker (cumulative billed input+output
	// tokens read from agents' transcripts). nil ⇒ unconfigured; gated by savingsOn
	// alongside the ledger, since it only feeds the savings report's denominator.
	// Recording is fail-open. See savings_routes.go / spend package.
	spend *spend.Store
	// savingsSamples gates the opt-in provenance capture at the emit sites (config
	// `savings_samples`, default off). When on, the record* helpers attach a
	// truncated raw/kept sample to the event; the store applies the 1-in-N
	// retention. Capture is fail-open like the rest of the savings path.
	savingsSamples bool
	// plugins dispatches lifecycle hook events to registered plugin executables
	// (#47). nil ⇒ the plugin system is off (the default); Dispatch is then a
	// no-op. Dispatch is always fail-open, so it never alters request flow.
	plugins *plugin.Dispatcher
	// apiDocs gates the public OpenAPI docs surface (/api/docs + the raw spec).
	// Default false on a bare Server literal; the daemon sets it from the
	// `api_docs` config setting (default on). See apidocs_routes.go.
	apiDocs bool
	// scheduler gates the native cron/at scheduler (#15) — its CRUD routes return
	// 403 and its reconcile loop is a no-op when off. Default false (opt-in via the
	// `scheduler_enabled` config setting). schedStore persists the schedules and
	// schedInterval is the reconcile tick. See schedule_routes.go / scheduler.go.
	scheduler     bool
	schedStore    *schedule.Store
	schedInterval time.Duration
	// backends is the agent-backend registry store (docs/specs/
	// 2026-08-06-backend-registry.md): the source of truth for which backends
	// exist, their tier, and which is default. nil ⇒ unconfigured (older wiring);
	// the registry routes added in later stages guard on it. Set by the daemon via
	// SetBackends after the startup reconcile.
	backends *backendstore.Store
	// autoApprovePersist persists a replaced auto-approve policy to the config
	// file (set by the daemon to config.WriteAutoApprove). nil ⇒ the PUT
	// /auto-approve/policy endpoint changes the live policy but does not persist.
	autoApprovePersist func(approval.Policy) error
	// autopilot is the autopilot master switch + per-plan run registry (S1). nil ⇒
	// the feature is unconfigured; GET /autopilot then reports disabled/empty and
	// POST /autopilot returns 403. See strict_autopilot.go / internal/autopilot.
	autopilot *autopilot.Controller
	// landHostFn builds the LandHost the `land` handler drives (autopilot.md §6).
	// nil ⇒ the real gh/git + check-rail host; tests inject a fake to exercise the
	// handler's resolution, ledger write, and error mapping without a live GitHub.
	landHostFn func(dir string) autopilot.LandHost
	// apNotifier is the operator notifier the autopilot guardian fans escalations
	// out to (desktop/webhook). nil ⇒ escalations are logged only. Set by the daemon
	// from config (SetAutopilotNotifier).
	apNotifier notify.Notifier

	// Config hot-reload (feature 3). appliedConfig is the last config ApplyConfig
	// installed (seeded at boot via SetBaselineConfig) — the "last-good" baseline
	// the restart-only diff compares against. reloadHooks are subsystem reconfigure
	// closures the daemon registers (AddReloadHook) for subsystems the Server does
	// not own directly: the lifecycle config swap, the notifier chain rebuild, and
	// the autopilot ControllerConfig re-derivation. All guarded by reloadMu.
	reloadMu      sync.Mutex
	appliedConfig config.Config
	reloadHooks   []func(config.Config)
}

// SetAutopilotController wires the autopilot Controller (docs/specs/autopilot.md).
// A nil controller leaves the feature unconfigured (GET reports disabled, POST
// returns 403). Named to avoid colliding with the generated SetAutopilot strict
// handler (POST /autopilot). It also injects the daemon-backed Runtime so that
// enabling a run spawns a real headless brain (S3): the brain lifecycle, the
// ctx-store ledger, the recovery digest sources, and owner notifications.
func (s *Server) SetAutopilotController(c *autopilot.Controller) {
	s.autopilot = c
	if c != nil {
		c.SetRuntime(autopilotRuntime{s: s})
	}
	// Wire the approval-routing seam (autopilot.md §8): while a run is active, an
	// autopilot-owned worker's unanswerable prompt (and tripped breaker) routes to
	// its brain instead of a human. A nil controller clears the seam so every
	// worker takes the normal human path again.
	if s.poller != nil {
		if c != nil {
			s.poller.Autopilot = autopilotApprovals{s: s}
		} else {
			s.poller.Autopilot = nil
		}
	}
}

// SetAutopilotNotifier wires the operator notifier the autopilot guardian fans
// escalations out to (desktop/webhook). A nil notifier leaves escalations
// log-only. Call before Start.
func (s *Server) SetAutopilotNotifier(n notify.Notifier) { s.apNotifier = n }

// SetScheduler wires the native scheduler (#15) and its config gate.
// enabled=false (or a nil store) makes the schedule endpoints return 403 and the
// reconcile loop a no-op. interval is the tick cadence (clamped to >=1s).
func (s *Server) SetScheduler(enabled bool, store *schedule.Store, interval time.Duration) {
	s.scheduler = enabled
	s.schedStore = store
	if interval < time.Second {
		interval = time.Minute
	}
	s.schedInterval = interval
}

// SetBackends wires the agent-backend registry store (docs/specs/
// 2026-08-06-backend-registry.md). A nil store leaves the registry unconfigured
// (the routes added in later stages then report it unavailable). Call before
// Start, after the startup detection reconcile.
func (s *Server) SetBackends(store *backendstore.Store) { s.backends = store }

// SetAPIDocs toggles the public OpenAPI documentation surface (#43): Swagger UI
// at /api/docs and the raw openapi.yaml. enabled=false makes those routes 404.
func (s *Server) SetAPIDocs(enabled bool) { s.apiDocs = enabled }

// SetAuth configures the bearer tokens required for remote access. An empty
// primary token disables authentication (the local-only default). When set,
// every request — including loopback — must present a token; see authorize. The
// optional readonlyToken grants scopeReadonly (reads only) and is only honored
// alongside a primary token (the caller enforces that invariant at startup). A
// per-IP auth-failure throttle is enabled alongside the primary token.
func (s *Server) SetAuth(token, readonlyToken string) {
	s.authToken = token
	s.readonlyToken = readonlyToken
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

// SetBudget configures the cost gate: the daily/weekly dollar caps and whether a
// spawn that has reached one warns (returns 428). enabled=false leaves spend
// tracking + the report live but never gates a spawn.
func (s *Server) SetBudget(enabled bool, dailyUSD, weeklyUSD float64) {
	s.pressMu.Lock()
	defer s.pressMu.Unlock()
	s.budgetGate = enabled
	s.budgetDailyUSD = dailyUSD
	s.budgetWeeklyUSD = weeklyUSD
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
	// SwitchRole re-injects the persona for sess.Role and relaunches the agent so
	// the new role takes effect (a plain resume re-injects nothing).
	SwitchRole(ctx context.Context, sess *store.Session) error
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
	Push(ctx context.Context, dir string, force bool) (lifecycle.PushResult, error)
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
	// MemoryPressure reads the current OS memory-pressure level (macOS sysctl /
	// Linux PSI; Normal on other platforms or any read error). Used by the
	// sampler loop and the spawn gate.
	MemoryPressure(ctx context.Context) (pressure.Level, error)
	// HotSwap retires the active agent process and launches a successor backend
	// in the same worktree with extracted context.
	HotSwap(ctx context.Context, sess *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error)
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
	fast, slow := s.writeFast, s.writeSlow
	if fast <= 0 {
		fast = writeTimeoutDur
	}
	if slow <= 0 {
		slow = slowWriteTimeoutDur
	}
	r.Use(writeTimeout(fast, slow))

	// Unauthenticated: a liveness probe (for tunnels/proxies) and the static UI.
	// The compiled SPA holds no secrets — serving its shell to anyone is what
	// lets a remote browser load the app and prompt for a token. All data- and
	// action-bearing routes live in the authenticated group below.
	r.Get("/healthz", s.handleHealthz)

	// The data/action API lives under /api/v1 so its paths never collide with the
	// SPA's client-side routes (/metrics, /pipelines, … are real browser URLs the
	// static catch-all must be free to serve). Every documented JSON operation is
	// served by the spec-generated strict chi server (oapi-codegen); *Server
	// implements oapi.StrictServerInterface in the strict_*.go files. The two
	// streaming routes (SSE event feed + WS attach) don't fit the strict
	// request/response model, so they are excluded from generation (see
	// oapi/config.yaml) and registered by hand alongside it.
	r.Group(func(ar chi.Router) {
		ar.Use(s.hostGuard)      // DNS-rebinding guard for the no-auth loopback default
		ar.Use(s.authMiddleware) // bearer-token gate for the API, SSE, and WS
		ar.Use(stashRequest)     // expose *http.Request to strict handlers (audit IP)
		ar.Get("/api/v1/events/stream", s.handleEventsStream)
		ar.Get("/api/v1/sessions/{id}/attach", s.handleAttach)
		ar.Get("/api/v1/cockpit/attach", s.handleCockpitAttach)
		strict := oapi.NewStrictHandlerWithOptions(s, nil, oapi.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  strictRequestError,
			ResponseErrorHandlerFunc: strictResponseError,
		})
		oapi.HandlerWithOptions(strict, oapi.ChiServerOptions{
			BaseRouter:       ar,
			ErrorHandlerFunc: strictParamError,
		})
	})

	s.registerAPIDocsRoutes(r) // public OpenAPI docs; explicit /api/docs* routes
	s.registerStatic(r)        // unauthenticated catch-all; must be last
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
