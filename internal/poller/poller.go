package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)

// classify derives a status from the latest pane capture + liveness.
// It only overrides the stored status when the pane (or staleness) gives a
// conclusive signal; otherwise it returns the existing status unchanged
// (hooks remain primary). sinceUpdate is how long the session has gone without
// any recorded activity (pane change, hook event, or status change).
//
// State detection is delegated to the agent's backend (b.DetectState): each
// backend recognizes its own pane signals, so this is no longer Claude-specific.
// For Claude the backend reproduces the original markers byte-for-byte ("esc to
// interrupt" ⇒ working, "❯"/"Do you want" ⇒ needs-input), so this is a
// behavior-preserving rewrite for the default backend; non-Claude backends now
// get real working/needs-input/idle detection instead of always degrading to a
// staleness guess. The rate-limit banner remains Claude-shaped (detectRateLimit)
// and simply never matches a non-Claude pane, degrading cleanly.
func classify(b agentbackend.Backend, s *store.Session, pane string, sessionAlive bool, sinceUpdate, stuckAfter time.Duration) store.Status {
	if !sessionAlive {
		return store.StatusOrphaned
	}

	var st agentbackend.State
	if b != nil {
		st = b.DetectState(pane)
	}

	// An agent that is actively streaming is working; a real limit banner only
	// appears once streaming has stopped, so working wins first and we never even
	// evaluate rate-limit detection on a live agent. This makes a stray "rate
	// limit" keyword in live output unable to misclassify it.
	if st == agentbackend.StateWorking {
		return store.StatusWorking
	}

	// Rate limit is checked before the waiting/idle heuristics so a banner is not
	// misread as waiting_for_input when its trailing prompt box is shown.
	if isLimited, _, _ := detectRateLimit(pane); isLimited {
		return store.StatusRateLimited
	}
	// A genuine approval prompt confirms waiting_for_input.
	// If the backend returns StateNeedsInput but there is no parseable approval
	// (e.g. an agent finished its task and sits at an empty prompt), classify as idle.
	if st == agentbackend.StateNeedsInput {
		ap, ok := b.ParseApproval(pane)
		if ok && ap != nil {
			return store.StatusWaitingForInput
		}
		return store.StatusIdle
	}
	// A backend that positively reports idle (Claude does not) is at rest.
	if st == agentbackend.StateIdle {
		return store.StatusIdle
	}
	// A session that still claims to be "working" but has shown no pane activity
	// for >= stuckAfter (and no "esc to interrupt" churn) is stuck or quietly
	// finished — downgrade to idle so it surfaces as needing attention rather
	// than masquerading as actively working. stuckAfter <= 0 disables this.
	if s.Status == store.StatusWorking && stuckAfter > 0 && sinceUpdate >= stuckAfter {
		return store.StatusIdle
	}
	return s.Status
}

// Deps is the poller's view of the world (store reads/writes + tmux probes).
type Deps interface {
	List(ctx context.Context) ([]*store.Session, error)
	// UpdateStatusIf swaps status from expected→next, reporting whether it took
	// effect. The poller uses the CAS form so it never overwrites a status a hook
	// changed between this tick's List and its write.
	UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error)
	UpdatePane(ctx context.Context, id, excerpt string) error
	UpdateSubject(ctx context.Context, id, subject string) error
	SessionAlive(ctx context.Context, tmuxName string) bool
	CapturePane(ctx context.Context, tmuxName string) (string, error)
	Summarize(ctx context.Context, s *store.Session) (string, error)
	// ExitCode returns the exit status recorded for the agent's shell, if any.
	ExitCode(ctx context.Context, id string) (code int, present bool)
	// FinalizeExit transitions the session to its terminal status from the exit
	// code (CAS on expected), recording the code (+ event for crashes).
	FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error)
	// ClearExit removes the consumed exit-file so it can't be re-read.
	ClearExit(ctx context.Context, id string)
	// ContextTokens returns the agent's current context-window occupancy read
	// from its transcript. ok=false when no model turn has been recorded yet.
	ContextTokens(ctx context.Context, s *store.Session) (tokens int, ok bool)
	// TranscriptUsage returns the agent's cumulative billed token usage
	// (input+output summed over every assistant turn) read from its transcript.
	// ok=false when no usage has been recorded yet or the transcript is
	// unreadable. Used for the real-spend denominator and the net compact cost.
	TranscriptUsage(ctx context.Context, s *store.Session) (inputTokens, outputTokens int, ok bool)
	// UpdateContext persists the gauge (tokens + state band).
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	// Compact sends "/compact" to the agent (only called when it is idle/waiting).
	Compact(ctx context.Context, s *store.Session) error
	// Interrupt sends an Escape keystroke to the agent's pane, cancelling the
	// in-flight turn so a busy agent drops to idle and can be /compact-ed. Used
	// only by the force-compact path (it discards the running turn's work).
	Interrupt(ctx context.Context, s *store.Session) error
	// Resume sends a prompt to a force-compacted agent so it picks its work back
	// up after the interrupt+compaction.
	Resume(ctx context.Context, s *store.Session, prompt string) error
	// StampCompact records that /compact was just sent (cooldown guard).
	StampCompact(ctx context.Context, id string) error
	// SendKeys sends a single key (e.g. numbered menu option) to the agent's tmux pane.
	SendKeys(ctx context.Context, tmuxSession, keys string) error
	// AskStatus sends the ambiguous-idle self-report query (the bracketed-paste +
	// Enter prompt path) to the agent, asking it once for its {status, details,
	// summary}. Used by the idle self-report fallback (askIdleStatus); a plain
	// prompt send, no different in mechanism from Resume.
	AskStatus(ctx context.Context, s *store.Session, prompt string) error
	// RecordEvent appends a durable event to the agent's record (used for health
	// anomalies the poller raises — OOM-suspected crashes, infinite loops,
	// pre-crash context warnings). A missing session is a soft no-op.
	RecordEvent(ctx context.Context, id string, ev store.Event) error
	// ProjectsDir is the backend transcript root (lifecycle's ProjectsDir), passed
	// to a backend's DiscoverSessionID — mirrors how TranscriptPath receives it.
	// Empty when transcript lookup is disabled.
	ProjectsDir() string
	// SetSessionID pins a discovered agent-generated session id to the session
	// (discover-then-pin). Persisted once, after which the exact id drives the
	// transcript path + resume in place of dir-scoping.
	SetSessionID(ctx context.Context, id, sessionID string) error
}

type Poller struct {
	deps Deps
	// Backend resolves the agent backend for a session, used for pane state
	// detection (classify) and approval parsing (tryAutoApprove). Defaults in New
	// to the agentbackend registry (with the Claude default for an empty/unknown
	// id); tests may override it with a fake backend.
	Backend        func(s *store.Session) agentbackend.Backend
	stuckAfter     time.Duration
	SummarizeAfter time.Duration        // throttle for subject refresh (0 = every change)
	lastSummary    map[string]time.Time // touched only by the tick goroutine
	// OnChange, if set, is called once after a tick that changed any session
	// (status or pane), and again from a summarizer worker when it refreshes a
	// subject. The daemon wires this to hub.publish for SSE.
	OnChange func()

	// OnTransition, if set, is called once per successful status swap with the
	// session and its old/new status (edge-triggered — once per transition, not
	// per tick). The daemon wires this to fire user notifications.
	OnTransition func(sess *store.Session, from, to store.Status)

	// OnAnomaly, if set, is called once per raised health anomaly (OOM-suspected
	// crash, infinite loop, pre-crash context). It is the notification seam — the
	// poller already records a durable event for every anomaly, so this is purely
	// best-effort user-facing alerting. The daemon wires it to its notifier.
	OnAnomaly func(sess *store.Session, a Anomaly)

	// Context-size guard config + hooks (set by the daemon after New). When
	// TokenGuard is false the whole check is skipped. CompactCooldown bounds how
	// often /compact may be auto-sent to one agent.
	//
	// TokenGuard/TokenWarn/TokenCrit/WarnAlert/AutoCompact/ForceCompact/
	// CompactResumePrompt are hot-reloadable (config live-reload): the daemon swaps
	// them atomically through SetContextGuard while the tick loop is running, so the
	// tick goroutine MUST read them via ctxGuard() (guarded by guardMu) rather than
	// touching the fields directly. Startup sets them once before Run (no reader yet),
	// so direct assignment there is still safe.
	guardMu     sync.RWMutex
	TokenGuard  bool
	TokenWarn   int
	TokenCrit   int
	WarnAlert   bool
	AutoCompact bool
	// ForceCompact is the global default for the destructive force-compact path:
	// interrupt a busy critical agent (Escape), /compact it once idle, then send
	// CompactResumePrompt. Per-agent Session.ForceCompact overrides this. Off by
	// default — it discards the agent's in-flight turn.
	ForceCompact        bool
	CompactResumePrompt string
	CompactCooldown     time.Duration
	CheckEvery          time.Duration // throttle for the per-agent transcript read
	// OnContextAlert, if set, fires once per upward threshold crossing.
	OnContextAlert func(sess *store.Session, state ctxtokens.State, tokens int)

	// RateLimitAutoResume mirrors rate_limit.auto_resume (set by the daemon after
	// New). When true the poller auto-selects the "Stop and wait for limit to
	// reset" choice on Claude's rate-limit menu so a limited agent parks itself;
	// when false the menu is left for a human. It gates only the menu selection —
	// the resume-after-clear scheduling lives in the daemon's RateLimitScheduler,
	// which shares the same toggle.
	RateLimitAutoResume bool

	// OnSaving, if set, records a token-savings event (the daemon wires it to the
	// savings ledger). The poller uses it for the auto-/compact win: when a
	// compaction it issued lands, the reclaimed context tokens are recorded as a
	// FeatureCompact saving. costTokens is the one-time output cost of generating
	// the summary (measured from the transcript usage delta straddling the
	// compaction, or 0 when unmeasurable) so the recorded saving is NET. Best-effort
	// and gate-aware on the receiving side.
	OnSaving func(feature, agent string, rawTokens, keptTokens, costTokens int)

	// Hot-swap threshold trigger (Task 3.4): when HandoverEnabled and OnHotSwap are
	// set, the poller signals a mid-session hot-swap the first time an agent's
	// context-window fill reaches the critical band (warden's existing near-full
	// signal, the same one that drives auto-/compact). It is edge-triggered per
	// critical episode via hotSwapFlagged, so the daemon's handler is called once —
	// not every tick — while the window stays full. The poller stays deliberately
	// thin: it detects the crossing and hands the session to OnHotSwap; the daemon
	// runs the full lifecycle.DecideHotSwap policy (fill %, provider-quota headroom,
	// cooldown) and performs the swap. nil OnHotSwap or a false HandoverEnabled makes
	// the whole path inert (the default), so existing deployments are unaffected.
	HandoverEnabled bool
	OnHotSwap       func(s *store.Session, tokens int)
	hotSwapFlagged  map[string]bool // per-agent: hot-swap already signalled this critical episode (tick goroutine only)

	// OnSpend, if set, records an agent's cumulative billed spend (input+output
	// tokens read from its transcript) so the report can express savings as a share
	// of REAL measured spend AND price it per model into the cost-governance rollup.
	// Called each context check with the session (for its model/repo) and the latest
	// cumulative reading; the daemon wires it to the spend tracker (which only ever
	// raises a session's figure). Best-effort and gate-aware on the receiving side.
	OnSpend func(s *store.Session, inputTokens, outputTokens int)

	lastCtxCheck map[string]time.Time // last context read per session (tick goroutine only)

	// pendingCompact tracks /compact sends whose reclaim hasn't shown up yet: a
	// compaction lands asynchronously, so the pre-compact reading is parked here
	// and reconciled against a later (lower) reading to measure the saving (tick
	// goroutine only).
	pendingCompact map[string]compactPending

	// Infinite-loop detection state (tick goroutine only). paneHistory holds the
	// last loopWindow distinct pane excerpts per session; loopFlagged remembers
	// whether a loop anomaly was already raised so it fires once per loop episode.
	paneHistory map[string][]string
	loopFlagged map[string]bool
	// preCrashFlagged remembers whether a "compact before crash" anomaly was
	// already raised for a session's current critical-context episode (tick
	// goroutine only), so the nudge fires once per episode, not per tick.
	preCrashFlagged map[string]bool

	// forceCompact tracks the per-agent force-compact state machine (interrupt →
	// await idle → /compact → await land → resume) for agents the force path is
	// driving this critical episode. Tick goroutine only.
	forceCompact map[string]fcState

	// AutoApprovePolicy is the auto-approve policy (from config): a default
	// allow/deny policy plus optional per-agent overrides (see approval.Policy).
	// Per-session Session.AutoApprove opts an agent into evaluation even when the
	// global master switch is off. Guarded by policyMu so the daemon's PUT
	// /auto-approve/policy handler can swap rules at runtime; read it through
	// autoApprovePolicy and write it through SetAutoApprovePolicy.
	AutoApprovePolicy approval.Policy
	policyMu          sync.RWMutex

	// approveBreaker halts auto-approval of an identical prompt that keeps
	// re-appearing after being approved (the approval isn't unblocking the
	// agent — e.g. it re-runs a failing command and re-asks forever). Internally
	// locked; shared by the approval worker and session teardown.
	approveBreaker *approval.Breaker

	// Autopilot routes approval decisions for autopilot-owned workers to their
	// run's brain instead of a human (docs/specs/autopilot.md §8): a prompt the
	// policy can't auto-answer is forwarded to the brain's mailbox (mirrored,
	// non-blocking, to the human inbox), and a tripped breaker escalates to the
	// brain rather than paging a human. nil ⇒ feature off; every worker takes the
	// normal human path. Set by the daemon (SetAutopilotController).
	Autopilot AutopilotApprovals
	// lastForward de-dupes brain-forwards: the prompt signature last forwarded per
	// worker, so an identical prompt seen on every tick forwards to the brain once
	// rather than on each poll. Guarded by fwdMu (touched by the approval worker
	// and by Prune on session teardown).
	lastForward map[string]string
	fwdMu       sync.Mutex

	// IdleSelfReport enables the ambiguous-idle self-report fallback
	// (docs/specs/2026-08-26-collaboration-groups.md §2.4(4)): the one case
	// pane-state cannot disambiguate — a monitored worker idle at its prompt
	// (finished vs. waiting for a human). When on, the poller asks the worker ONCE,
	// on its transition to idle, for {status, details, summary}. Off by default; the
	// daemon enables it. Restricted to monitored pipeline workers and made one-shot
	// per session (idleQueried) so it never degrades into a poll loop.
	IdleSelfReport bool
	// idleQueried marks sessions already asked to self-report on their idle
	// transition. Sticky per session (reset only on teardown via pruneSummaryState,
	// NOT when the agent leaves idle) so the query fires at most once and never
	// re-fires on the working→idle cycle the query itself induces. Tick goroutine only.
	idleQueried map[string]bool

	// ApprovalEvents is a buffered channel for approval opportunities.
	// Published when: (1) status transitions to waiting_for_input, OR
	// (2) pane changes while already in waiting_for_input.
	// Consumed by the approval worker goroutine.
	ApprovalEvents chan ApprovalEvent

	// Summarization runs `claude -p`, which is slow, so it is dispatched to
	// background workers rather than blocking the tick loop. mu guards inflight;
	// wg tracks live workers so Run can drain them on shutdown.
	mu       sync.Mutex
	inflight map[string]struct{} // session ids with a summarizer currently running
	wg       sync.WaitGroup
}

// ApprovalEvent represents a potential auto-approval opportunity.
type ApprovalEvent struct {
	Session *store.Session // snapshot at event time
	Pane    string         // pane content that triggered the event
}

// AutopilotApprovals routes approval decisions for autopilot-owned workers to
// their run's brain instead of a human (docs/specs/autopilot.md §8). The daemon
// implements it over the autopilot Controller (which run is active + its brain)
// and the mailbox; a nil poller.Autopilot leaves every worker on the normal
// human path.
type AutopilotApprovals interface {
	// BrainFor returns the brain agent id owning worker session s while its run is
	// active; ok=false when s is not an active autopilot-owned worker (or is the
	// brain itself), in which case the normal human-escalation path applies.
	BrainFor(s *store.Session) (brainID string, ok bool)
	// Forward delivers a prompt the auto-approve policy could not answer to the
	// brain's mailbox and mirrors a non-blocking copy to the human inbox
	// (visibility + audit). The poller de-dupes by prompt, so Forward is called at
	// most once per distinct prompt per worker and need not throttle itself.
	Forward(ctx context.Context, brainID string, worker *store.Session, reason string)
}

// compactPending is a /compact awaiting its reclaim: pre is the context-token
// reading captured the moment warden sent /compact, at is when it was sent (used
// to abandon a compaction that never visibly landed so it can't later be
// credited to an unrelated drop). preOut is the agent's cumulative billed output
// tokens at that same moment; outOK records whether it was measurable. The output
// the transcript bills between then and the landing is the summary-generation
// cost, netted out of the recorded saving so the figure is a true NET reclaim.
type compactPending struct {
	pre    int
	preOut int
	outOK  bool
	at     time.Time
}

// fcPhase is the stage of the force-compact state machine for one agent.
type fcPhase int

const (
	// fcInterrupting: warden sent Escape and is waiting for the busy agent to drop
	// to idle/waiting so it can be /compact-ed.
	fcInterrupting fcPhase = iota
	// fcAwaitLand: /compact was sent (parked in pendingCompact); warden is waiting
	// for the compaction to land before sending the resume prompt.
	fcAwaitLand
)

// fcState is one agent's position in the force-compact machine. at is when the
// current phase began (used to abandon an interrupt that never takes).
type fcState struct {
	phase fcPhase
	at    time.Time
}

// resolveBackend maps a session to its backend via the registry, falling back to
// the Claude default for an empty or unrecognized backend id (back-compat). The
// registry is populated at process start by the backends package's init, which
// the daemon imports transitively through lifecycle.
func resolveBackend(s *store.Session) agentbackend.Backend {
	if s != nil {
		if b, err := agentbackend.Get(s.Backend); err == nil && b != nil {
			return b
		}
	}
	return agentbackend.Default()
}

// backendFor resolves the backend for a session through the configured resolver
// (Backend field), defaulting to the registry when unset.
func (p *Poller) backendFor(s *store.Session) agentbackend.Backend {
	if p.Backend != nil {
		return p.Backend(s)
	}
	return resolveBackend(s)
}

// discoverSessionID lazily pins the agent-generated session id for a non-pinning
// backend (Caps.SessionIDControl=false). It runs only while the session id is
// still empty: a pinning backend (Claude) already carries warden's minted id at
// spawn, so the SessionIDControl guard means this path NEVER runs for it. For a
// non-pinning backend that implements SessionIDDiscoverer, it asks the backend to
// find the id from the on-disk transcript and persists it once; backends without
// the optional interface are skipped (they keep dir-scoping). The discovered id is
// also written back onto the in-memory snapshot so this tick's later transcript
// reads use the exact id immediately.
func (p *Poller) discoverSessionID(ctx context.Context, s *store.Session) {
	if s.ClaudeSessionID != "" {
		return // already pinned (warden-minted or previously discovered)
	}
	b := p.backendFor(s)
	if b.Capabilities().SessionIDControl {
		return // pinning backend mints at spawn; never discovered
	}
	d, ok := b.(agentbackend.SessionIDDiscoverer)
	if !ok {
		return // backend keeps dir-scoping (no discovery support yet)
	}
	id, ok := d.DiscoverSessionID(p.deps.ProjectsDir(), s.Workdir)
	if !ok || id == "" {
		return // transcript not written yet — retry on a later tick
	}
	if err := p.deps.SetSessionID(ctx, s.ID, id); err != nil {
		slog.Warn("poller: pin discovered session id failed", "agent", s.ID, "err", err)
		return
	}
	s.ClaudeSessionID = id // reflect on the snapshot for this tick's transcript reads
	slog.Info("poller: pinned discovered session id", "agent", s.ID, "session_id", id)
}

func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{
		deps:            d,
		Backend:         resolveBackend,
		stuckAfter:      stuckAfter,
		SummarizeAfter:  2 * time.Minute,
		lastSummary:     map[string]time.Time{},
		inflight:        map[string]struct{}{},
		lastCtxCheck:    map[string]time.Time{},
		pendingCompact:  map[string]compactPending{},
		paneHistory:     map[string][]string{},
		loopFlagged:     map[string]bool{},
		preCrashFlagged: map[string]bool{},
		hotSwapFlagged:  map[string]bool{},
		forceCompact:    map[string]fcState{},
		idleQueried:     map[string]bool{},
		approveBreaker:  approval.NewBreaker(),
		lastForward:     map[string]string{},
		CheckEvery:      20 * time.Second,
		CompactCooldown: 2 * time.Minute,
		ApprovalEvents:  make(chan ApprovalEvent, 100),
	}
}

// summaryTimeout bounds a single `claude -p` summary call. Without it, a hung
// model call holds the session's inflight flag indefinitely (it is cleared only
// when runSummary returns), permanently suppressing that session's subject
// refreshes. Var (not const) so tests can shrink it.
var summaryTimeout = 60 * time.Second

func isTerminal(s store.Status) bool {
	switch s {
	case store.StatusDone, store.StatusErrored, store.StatusOrphaned:
		return true
	}
	return false
}

// autoApprovePolicy returns the live auto-approve policy under the read lock.
func (p *Poller) autoApprovePolicy() approval.Policy {
	p.policyMu.RLock()
	defer p.policyMu.RUnlock()
	return p.AutoApprovePolicy
}

// AutoApprovePolicySnapshot returns the live auto-approve policy (default +
// per-agent overrides). Used by the daemon's GET /auto-approve/policy handler.
func (p *Poller) AutoApprovePolicySnapshot() approval.Policy {
	return p.autoApprovePolicy()
}

// SessionAlive reports whether name's tmux session is currently alive, via the
// same liveness check the tick loop uses. Exposed so other daemon components
// (the tombstone reaper) can reconfirm liveness with an authoritative,
// just-now check before treating a stale orphaned record as genuinely dead —
// orphaned status can lag a session's real state for one tick.
func (p *Poller) SessionAlive(ctx context.Context, name string) bool {
	return p.deps.SessionAlive(ctx, name)
}

// SetAutoApprovePolicy atomically swaps the live auto-approve policy so the
// daemon's PUT /auto-approve/policy handler can change rules without a restart.
func (p *Poller) SetAutoApprovePolicy(pol approval.Policy) {
	p.policyMu.Lock()
	p.AutoApprovePolicy = pol
	p.policyMu.Unlock()
}

// ctxGuardSnapshot is a consistent read of the hot-reloadable context/token guard
// knobs. Taken under guardMu so a live config reload (SetContextGuard) can swap
// them while the tick goroutine reads them.
type ctxGuardSnapshot struct {
	Guard         bool
	Warn, Crit    int
	WarnAlert     bool
	AutoCompact   bool
	ForceCompact  bool
	CompactResume string
}

// ctxGuard returns a coherent snapshot of the context-guard knobs under the read
// lock. The tick goroutine calls this instead of reading the fields directly so a
// concurrent SetContextGuard (config reload) is race-free.
func (p *Poller) ctxGuard() ctxGuardSnapshot {
	p.guardMu.RLock()
	defer p.guardMu.RUnlock()
	return ctxGuardSnapshot{
		Guard:         p.TokenGuard,
		Warn:          p.TokenWarn,
		Crit:          p.TokenCrit,
		WarnAlert:     p.WarnAlert,
		AutoCompact:   p.AutoCompact,
		ForceCompact:  p.ForceCompact,
		CompactResume: p.CompactResumePrompt,
	}
}

// SetContextGuard atomically swaps the context/token guard knobs so a live config
// reload of the tokens.* thresholds (guard on/off, warn/critical bands, warn
// alerting, auto-/force-compact, the compact resume prompt) takes effect on the
// next tick without a daemon restart. Safe for concurrent use with the tick loop.
func (p *Poller) SetContextGuard(guard bool, warn, crit int, warnAlert, autoCompact, forceCompact bool, compactResume string) {
	p.guardMu.Lock()
	defer p.guardMu.Unlock()
	p.TokenGuard = guard
	p.TokenWarn = warn
	p.TokenCrit = crit
	p.WarnAlert = warnAlert
	p.AutoCompact = autoCompact
	p.ForceCompact = forceCompact
	p.CompactResumePrompt = compactResume
}

// routeToBrain forwards an unanswerable worker prompt to its run's brain,
// de-duped so an identical prompt seen on every tick forwards once (autopilot.md
// §8). It returns true when the worker is an active autopilot-owned agent — the
// caller then suppresses the human-escalation path, since no autopilot worker
// ever waits on a human. It is a no-op returning false for every ordinary agent
// (Autopilot unset, or the worker isn't autopilot-owned).
func (p *Poller) routeToBrain(ctx context.Context, s *store.Session, sig, reason string) bool {
	if p.Autopilot == nil {
		return false
	}
	brainID, ok := p.Autopilot.BrainFor(s)
	if !ok {
		return false
	}
	// Forward once per distinct prompt: the same prompt re-observed each tick
	// stays "handled" (suppress the human path) but is not re-sent to the brain.
	p.fwdMu.Lock()
	dup := p.lastForward[s.ID] == sig
	if !dup {
		p.lastForward[s.ID] = sig
	}
	p.fwdMu.Unlock()
	if dup {
		return true
	}
	slog.Info("autopilot: forwarding worker prompt to brain", "agent", s.ID, "brain", brainID, "reason", reason)
	p.Autopilot.Forward(ctx, brainID, s, reason)
	return true
}

// tryAutoApprove attempts to auto-approve a recognized prompt by pressing its
// least-privilege affirmative ("yes") option. Only attempts auto-approval if:
//   - the effective policy is enabled OR session.AutoApprove is true (the
//     participate-gate). The effective policy is resolved per-agent: a per-agent
//     override (keyed by agent name or id) replaces the default's rules.
//   - The pane content parses as a recognized prompt (approval.Parse ok=true)
//
// A recognized prompt naming a destructive/irreversible action is blocked
// unconditionally (the destructive guard runs first and is not configurable).
//
// Rule evaluation is backward-compatible: when the effective policy has NO allow
// or deny rules, the legacy on/off behavior applies (approve every recognized,
// non-destructive prompt) so a bare `auto_approve: true` keeps working. When any
// rule is present the prompt must match the allow/deny policy (Decide): deny wins
// over allow, and an empty allow list approves nothing. Prompts with no
// affirmative option are skipped, as are sticky-only "don't ask again"
// affirmatives unless the effective policy's AllowSticky is set.
//
// Idempotent and safe to call repeatedly on the same prompt: an unrecognized or
// already-dismissed prompt is a logged no-op.
func (p *Poller) tryAutoApprove(ctx context.Context, s *store.Session, pane string) {
	// Resolve the effective policy for this agent (per-agent override or default).
	pol := p.autoApprovePolicy().For(s.Name, s.ID)

	// participate-gate: master switch (default or per-agent) OR per-session opt-in.
	if !pol.Enabled && !s.AutoApprove {
		return
	}

	// Parse the approval through the agent's backend (each backend recognizes its
	// own prompt UI; the Claude backend delegates to approval.Parse). The neutral
	// agentbackend.Approval is mapped onto approval.Approval so the policy engine
	// (IsDestructive / Decide) is unchanged.
	ap, ok := p.backendFor(s).ParseApproval(pane)
	if !ok || ap == nil || len(ap.Options) == 0 {
		slog.Debug("auto-approve skipped: unrecognized prompt", "agent", s.ID)
		return
	}
	a := approval.Approval{
		Action:            ap.Action,
		Question:          ap.Question,
		Options:           ap.Options,
		SelectedIdx:       ap.SelectedIdx,
		AffirmativeIdx:    ap.AffirmativeIdx,
		AffirmativeSticky: ap.AffirmativeSticky,
	}

	// Signature of this prompt, shared by the circuit breaker and the brain-forward
	// de-dupe so both count "the same prompt" identically.
	sig := a.Action + "\x00" + a.Question

	// Never auto-confirm a destructive/irreversible action — this guard runs BEFORE
	// Decide, so no allow rule can ever un-block it. For an autopilot-owned worker
	// the blocked prompt is handed to the brain rather than left for a human (§8);
	// for any other agent it stays unanswered (surfaces as waiting_for_input).
	if bad, marker := approval.IsDestructive(a); bad {
		slog.Warn("auto-approve BLOCKED: destructive action", "agent", s.ID, "marker", marker)
		p.routeToBrain(ctx, s, sig, "destructive prompt blocked ("+marker+"): "+a.Action)
		return
	}
	// Evaluate against the allow/deny policy only when rules are configured. With
	// no rules the legacy on/off behavior stands (approve any recognized,
	// non-destructive prompt), keeping `auto_approve: true` backward-compatible.
	if pol.HasRules() {
		if d := pol.Decide(a); !d.Approve {
			slog.Debug("auto-approve skipped by policy", "agent", s.ID, "reason", d.Reason)
			p.routeToBrain(ctx, s, sig, "auto-approve policy could not answer ("+d.Reason+"): "+a.Action)
			return
		}
	}
	if a.AffirmativeIdx == 0 {
		slog.Debug("auto-approve skipped: no affirmative option", "agent", s.ID)
		p.routeToBrain(ctx, s, sig, "prompt has no auto-approvable option: "+a.Action)
		return
	}
	if a.AffirmativeSticky && !pol.AllowSticky {
		slog.Debug("auto-approve skipped: only a sticky affirmative (allow_sticky off)", "agent", s.ID)
		p.routeToBrain(ctx, s, sig, "only a sticky 'don't ask again' affirmative (allow_sticky off): "+a.Action)
		return
	}

	// Circuit breaker: an identical prompt that keeps re-appearing after being
	// approved means approving is not unblocking the agent — stop feeding the loop.
	// An autopilot-owned worker hands the loop to its brain instead of a human
	// (§8: no human escalation entry); any other agent escalates to a human and the
	// prompt sits unanswered (surfaces as waiting_for_input).
	maxRepeats := pol.EffectiveMaxRepeats()
	if ok, trippedNow := p.approveBreaker.Allow(s.ID, sig, maxRepeats); !ok {
		if trippedNow {
			slog.Warn("auto-approve circuit breaker tripped", "agent", s.ID, "action", a.Action, "repeats", maxRepeats)
			detail := fmt.Sprintf("auto-approve halted: the identical prompt (%s) was approved %d times in a row without unblocking the agent",
				a.Action, maxRepeats)
			if !p.routeToBrain(ctx, s, sig, detail) {
				p.raiseAnomaly(ctx, s, Anomaly{
					Kind:   anomalyApprovalLoop,
					Detail: detail + " — answer it manually or interrupt the agent",
				})
			}
		}
		return
	}

	key := strconv.Itoa(a.AffirmativeIdx)
	if err := p.deps.SendKeys(ctx, s.TmuxSession, key); err != nil {
		slog.Warn("auto-approve failed to send keys", "agent", s.ID, "err", err)
		return
	}

	slog.Info("auto-approved", "agent", s.ID, "option", key, "label", a.Options[a.AffirmativeIdx-1])
	if p.OnChange != nil {
		p.OnChange()
	}
}

// idleSelfReportQuery is the one-shot prompt warden sends a monitored worker the
// first time it transitions to idle-at-prompt — the single case pane-state can't
// disambiguate (finished vs. waiting for a human). It asks for the
// {status, details, summary} contract (design §2.4(4)) and points a finished
// worker at the deterministic done-signal so its reply is machine-captured. Var
// (not const) so tests can shrink/inspect it.
var idleSelfReportQuery = "warden: you appear idle at your prompt. If you have FINISHED your task, run " +
	"`wd job done --summary '<one line>'`. Otherwise reply with one line of JSON describing your state: " +
	`{"status":"working|blocked|done","details":"<what you're waiting on or why you're stuck>","summary":"<one-line result>"}.`

// askIdleStatus fires the ambiguous-idle self-report exactly once per session, on
// its transition to idle-at-prompt (design §2.4(4): ask once, never on a poll
// loop). It is gated on IdleSelfReport, restricted to monitored pipeline workers
// (a human-driven agent's idle needs no disambiguation — its operator is present),
// and guarded by idleQueried so it never re-fires — including on the working→idle
// cycle the query itself induces. The guard is set only after a successful send so
// a transient tmux failure retries on a later idle tick. Tick goroutine only.
func (p *Poller) askIdleStatus(ctx context.Context, s *store.Session) {
	if !p.IdleSelfReport {
		return
	}
	if s.PipelineID == "" {
		return
	}
	if p.idleQueried[s.ID] {
		return
	}
	if err := p.deps.AskStatus(ctx, s, idleSelfReportQuery); err != nil {
		slog.Warn("poller: idle self-report query failed", "agent", s.ID, "err", err)
		return
	}
	p.idleQueried[s.ID] = true
	slog.Info("poller: sent ambiguous-idle self-report query", "agent", s.ID)
}

// menuVerifyDelay is how long tryLimitMenu waits after its first keystroke
// before re-capturing the pane to confirm the menu cleared. A menu redraw is
// near-instant; the small pause avoids a false "still showing" read that would
// trigger the fallback unnecessarily. Overridable in tests.
var menuVerifyDelay = 600 * time.Millisecond

// tryLimitMenu auto-selects the "Stop and wait for limit to reset" choice on
// Claude's rate-limit menu (the safe option that parks the agent until the
// limit clears, vs. "Upgrade your plan"). It is a no-op unless the pane is that
// specific menu and rate_limit.auto_resume is on. Selecting the wait option
// dismisses the menu so the limit banner surfaces on a later tick, at which
// point the agent classifies as rate_limited and the daemon's scheduler drives
// the resume-after-clear.
//
// The menu's footer reads "Enter to confirm" and the safe option is normally
// pre-highlighted (❯), so the answer sequence is Enter-first: send Enter, then
// re-capture and verify the menu cleared. Only if it persists does it fall back
// to the explicit "<n> then Enter" — the number moves the cursor to the wait
// option in case it wasn't the highlighted one. When the wait option is not the
// highlighted one to begin with, a bare Enter would confirm the WRONG choice
// (e.g. Upgrade), so that case skips straight to the number.
func (p *Poller) tryLimitMenu(ctx context.Context, s *store.Session, pane string) {
	if !p.RateLimitAutoResume {
		return
	}
	waitIdx, highlighted, ok := LimitMenuSelection(pane)
	if !ok {
		return
	}
	numKey := strconv.Itoa(waitIdx)

	// Phase 1: Enter when the wait option is already highlighted; otherwise jump
	// to it by number (which both selects and, on Claude's menus, confirms).
	first := "Enter"
	if !highlighted {
		first = numKey
	}
	if err := p.deps.SendKeys(ctx, s.TmuxSession, first); err != nil {
		slog.Warn("rate-limit menu: failed to select wait option", "agent", s.ID, "err", err)
		return
	}
	slog.Info("rate-limit menu: auto-selected wait-for-reset", "agent", s.ID, "key", first, "option", numKey)

	// Phase 2: verify the menu actually cleared; if it is still showing, send the
	// explicit number + Enter as a fallback.
	if p.limitMenuStillShowing(ctx, s) {
		slog.Info("rate-limit menu: still showing after first keystroke, sending number+Enter fallback", "agent", s.ID, "option", numKey)
		if err := p.deps.SendKeys(ctx, s.TmuxSession, numKey); err != nil {
			slog.Warn("rate-limit menu: fallback number send failed", "agent", s.ID, "err", err)
		} else if err := p.deps.SendKeys(ctx, s.TmuxSession, "Enter"); err != nil {
			slog.Warn("rate-limit menu: fallback Enter send failed", "agent", s.ID, "err", err)
		}
	}

	if p.OnChange != nil {
		p.OnChange()
	}
}

// limitMenuStillShowing re-captures the pane after a short settle delay and
// reports whether Claude's rate-limit menu is still displayed — i.e. the first
// keystroke did not dismiss it. A capture error is treated as "not showing" so a
// transient tmux hiccup never triggers a spurious fallback keystroke into a pane
// whose real state we couldn't read.
func (p *Poller) limitMenuStillShowing(ctx context.Context, s *store.Session) bool {
	if menuVerifyDelay > 0 {
		select {
		case <-time.After(menuVerifyDelay):
		case <-ctx.Done():
			return false
		}
	}
	pane, err := p.deps.CapturePane(ctx, s.TmuxSession)
	if err != nil {
		return false
	}
	_, ok := LimitMenuOption(pane)
	return ok
}

// runApprovalWorker consumes approval events and attempts auto-approval.
// Runs until ctx is cancelled.
func (p *Poller) runApprovalWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-p.ApprovalEvents:
			p.tryAutoApprove(ctx, event.Session, event.Pane)
		}
	}
}

func (p *Poller) tick(ctx context.Context) error {
	sessions, err := p.deps.List(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	changed := false
	for _, s := range sessions {
		if isTerminal(s.Status) {
			// Reap any exit-file left by the clean-exit path (SessionEnd hook set
			// done before the poller read the file); errored/orphaned already
			// cleared theirs in the finalize branch, making this a no-op there.
			p.deps.ClearExit(ctx, s.ID)
			// For errored or orphaned agents: if the tmux session is still alive the
			// prior classification was stale (e.g. a rate-limit resume race, or the
			// daemon restarting mid-poll and mis-tagging a live session as orphaned)
			// — fall through to reclassify so the TUI reflects the real state. done
			// is always terminal; errored/orphaned are only skipped while dead.
			if (s.Status != store.StatusErrored && s.Status != store.StatusOrphaned) || !p.deps.SessionAlive(ctx, s.TmuxSession) {
				continue
			}
		}
		// Exit-file is authoritative: if the agent's shell recorded an exit code,
		// finalize from it (CAS so a SessionEnd hook that already set done wins)
		// and skip liveness/pane classification this tick.
		if code, ok := p.deps.ExitCode(ctx, s.ID); ok {
			next := store.StatusDone
			if code != 0 {
				next = store.StatusErrored
			}
			swapped, err := p.deps.FinalizeExit(ctx, s.ID, s.Status, next, code)
			if err != nil {
				slog.Warn("poller: finalize exit failed", "agent", s.ID, "err", err)
				continue // leave the file; retry next tick
			}
			p.deps.ClearExit(ctx, s.ID) // consumed (clear even if CAS lost — the file is stale)
			if swapped {
				changed = true
				// A crash carrying the OOM-kill signature gets an enrichment event
				// + notification beyond the generic exit event FinalizeExit wrote.
				if next == store.StatusErrored {
					if a, ok := crashAnomaly(code); ok {
						p.raiseAnomaly(ctx, s, a)
					}
				}
				if p.OnTransition != nil {
					p.OnTransition(s, s.Status, next)
				}
			}
			continue
		}
		// A terminal (Kind==terminal) is a plain shell, not an AI agent: it has no
		// transcript to summarize, no approval or rate-limit menu to answer, and no
		// meaningful working/idle/waiting classification. Keep only what applies to
		// any tracked pane — exit finalization (handled above) and a live pane
		// excerpt for display — and skip every AI-reasoning branch below
		// (discover-session-id, classify, summarize, auto-approve, limit-menu,
		// context-guard). This guard is load-bearing: a terminal's empty Backend
		// resolves to the default backend (Claude), which would otherwise drive all
		// of those against a shell.
		if s.IsTerminal() {
			if p.deps.SessionAlive(ctx, s.TmuxSession) {
				if captured, err := p.deps.CapturePane(ctx, s.TmuxSession); err == nil {
					if excerpt := lastLines(captured, 20); excerpt != s.LastPaneExcerpt {
						_ = p.deps.UpdatePane(ctx, s.ID, excerpt)
						changed = true
					}
				}
			}
			continue
		}
		alive := p.deps.SessionAlive(ctx, s.TmuxSession)
		// Discover-then-pin: a non-pinning backend mints its own session id at
		// launch, so ClaudeSessionID starts empty (dir-scoped fallback). Once the
		// agent has written its transcript, discover the real id and persist it once
		// — after which the transcript path + resume key off the exact id.
		if alive {
			p.discoverSessionID(ctx, s)
		}
		var pane string
		paneChanged := false
		captureOK := true
		if alive {
			captured, err := p.deps.CapturePane(ctx, s.TmuxSession)
			if err != nil {
				// Transient capture failure: don't record an empty excerpt or
				// let an empty pane drive classification this tick.
				captureOK = false
			} else {
				pane = captured
				if excerpt := lastLines(pane, 20); excerpt != s.LastPaneExcerpt {
					_ = p.deps.UpdatePane(ctx, s.ID, excerpt)
					changed = true
					paneChanged = true

					// The pane is actively churning — feed loop detection (the
					// stuck timer only catches a STALE pane, never a busy loop).
					p.trackLoop(ctx, s, excerpt)

					// A freshly drawn rate-limit menu ("Stop and wait for limit to
					// reset" / "Upgrade your plan") is auto-answered here so the agent
					// parks instead of sitting on an unanswered menu. Gated on
					// auto_resume; a no-op for any other pane.
					p.tryLimitMenu(ctx, s, pane)

					// Publish approval event if already waiting
					if s.Status == store.StatusWaitingForInput && pane != "" {
						p.publishApprovalEvent(s, pane)
					}
				}
			}
		}
		// Reclassify only when we have a fresh signal: either the session is dead
		// (orphaned, pane-independent) or we captured the pane successfully.
		if !alive || captureOK {
			next := classify(p.backendFor(s), s, pane, alive, time.Since(s.UpdatedAt), p.stuckAfter)
			idleNow := false
			if next != s.Status {
				// CAS on the snapshot's status: if a hook changed it since List,
				// the swap is skipped and the hook's newer status stands.
				if ok, err := p.deps.UpdateStatusIf(ctx, s.ID, s.Status, next); err != nil {
					slog.Warn("poller: status update failed", "agent", s.ID, "err", err)
				} else if ok {
					changed = true
					if p.OnTransition != nil {
						p.OnTransition(s, s.Status, next)
					}
					// Publish approval event on transition to waiting_for_input
					if next == store.StatusWaitingForInput && pane != "" {
						p.publishApprovalEvent(s, pane)
					}
					idleNow = next == store.StatusIdle
				}
			} else {
				idleNow = s.Status == store.StatusIdle
			}
			// Ambiguous-idle self-report (design §2.4(4)): the one case pane-state
			// can't disambiguate. Fire once when the worker is confirmed idle-at-prompt
			// (whether this tick moved it there or it was already idle); askIdleStatus
			// is gated + one-shot, so this never becomes a poll loop.
			if idleNow {
				p.askIdleStatus(ctx, s)
			}
		}
		if alive && paneChanged && now.Sub(p.lastSummary[s.ID]) >= p.SummarizeAfter {
			p.dispatchSummary(ctx, s, now)
		}
		if p.ctxGuard().Guard && alive && p.CheckEvery >= 0 && now.Sub(p.lastCtxCheck[s.ID]) >= p.CheckEvery {
			p.lastCtxCheck[s.ID] = now
			p.checkContext(ctx, s, now)
		}
	}
	p.pruneSummaryState(sessions)
	if changed && p.OnChange != nil {
		p.OnChange()
	}
	return nil
}

// pruneSummaryState drops lastSummary entries for sessions no longer in the
// store (archived/deleted), so the throttle map can't grow without bound over a
// long-running daemon. Called only from the tick goroutine, which owns the map.
func (p *Poller) pruneSummaryState(sessions []*store.Session) {
	if len(p.lastSummary) == 0 && len(p.lastCtxCheck) == 0 &&
		len(p.pendingCompact) == 0 && len(p.paneHistory) == 0 &&
		len(p.loopFlagged) == 0 && len(p.preCrashFlagged) == 0 &&
		len(p.hotSwapFlagged) == 0 &&
		len(p.forceCompact) == 0 && len(p.idleQueried) == 0 &&
		p.approveBreaker.Len() == 0 {
		return
	}
	live := make(map[string]struct{}, len(sessions))
	for _, s := range sessions {
		live[s.ID] = struct{}{}
	}
	for id := range p.lastSummary {
		if _, ok := live[id]; !ok {
			delete(p.lastSummary, id)
		}
	}
	for id := range p.lastCtxCheck {
		if _, ok := live[id]; !ok {
			delete(p.lastCtxCheck, id)
		}
	}
	for id := range p.pendingCompact {
		if _, ok := live[id]; !ok {
			delete(p.pendingCompact, id)
		}
	}
	for id := range p.paneHistory {
		if _, ok := live[id]; !ok {
			delete(p.paneHistory, id)
		}
	}
	for id := range p.loopFlagged {
		if _, ok := live[id]; !ok {
			delete(p.loopFlagged, id)
		}
	}
	for id := range p.preCrashFlagged {
		if _, ok := live[id]; !ok {
			delete(p.preCrashFlagged, id)
		}
	}
	for id := range p.hotSwapFlagged {
		if _, ok := live[id]; !ok {
			delete(p.hotSwapFlagged, id)
		}
	}
	for id := range p.forceCompact {
		if _, ok := live[id]; !ok {
			delete(p.forceCompact, id)
		}
	}
	for id := range p.idleQueried {
		if _, ok := live[id]; !ok {
			delete(p.idleQueried, id)
		}
	}
	p.approveBreaker.Prune(live)
	p.fwdMu.Lock()
	for id := range p.lastForward {
		if _, ok := live[id]; !ok {
			delete(p.lastForward, id)
		}
	}
	p.fwdMu.Unlock()
}

// dispatchSummary launches a background summarizer for s unless one is already
// running for it. It is called only from the tick goroutine, so lastSummary is
// updated synchronously here (before the worker starts) to keep the throttle
// honest even while the slow `claude -p` call is still in flight.
func (p *Poller) dispatchSummary(ctx context.Context, s *store.Session, now time.Time) {
	p.mu.Lock()
	if _, busy := p.inflight[s.ID]; busy {
		p.mu.Unlock()
		return
	}
	p.inflight[s.ID] = struct{}{}
	p.mu.Unlock()

	p.lastSummary[s.ID] = now
	p.wg.Add(1)
	go p.runSummary(ctx, s)
}

// runSummary produces and persists a fresh subject for s, then notifies SSE.
// It runs off the tick loop so a slow model call never stalls status polling.
func (p *Poller) runSummary(ctx context.Context, s *store.Session) {
	defer p.wg.Done()
	defer func() {
		p.mu.Lock()
		delete(p.inflight, s.ID)
		p.mu.Unlock()
	}()

	// Bound the slow model call so a hang can't latch inflight forever.
	sctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	subj, err := p.deps.Summarize(sctx, s)
	if err != nil {
		slog.Debug("poller: summarize failed", "agent", s.ID, "err", err)
		return
	}
	if subj == "" || subj == s.Subject {
		return
	}
	if err := p.deps.UpdateSubject(ctx, s.ID, subj); err != nil {
		slog.Warn("poller: subject update failed", "agent", s.ID, "err", err)
		return
	}
	if p.OnChange != nil {
		p.OnChange()
	}
}

// Run ticks every interval until ctx is cancelled.
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	// Start approval worker
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runApprovalWorker(ctx)
	}()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain in-flight summarizers + approval worker; ctx cancellation
			// already aborts their work, so this returns promptly.
			p.wg.Wait()
			return
		case <-t.C:
			if err := p.tick(ctx); err != nil {
				slog.Warn("poller: tick failed", "err", err)
			}
		}
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// raiseAnomaly records a durable "anomaly" event for the agent and fires the
// optional OnAnomaly notification hook. The event is the authoritative surface
// (visible in `show`/TUI even with no notifier); OnAnomaly is best-effort alerting.
func (p *Poller) raiseAnomaly(ctx context.Context, s *store.Session, a Anomaly) {
	if err := p.deps.RecordEvent(ctx, s.ID, store.Event{Type: "anomaly", Detail: a.Detail}); err != nil {
		slog.Warn("poller: record anomaly event failed", "agent", s.ID, "kind", a.Kind, "err", err)
	}
	if p.OnAnomaly != nil {
		p.OnAnomaly(s, a)
	}
}

// trackLoop appends the latest pane excerpt to the agent's rolling history and
// raises a one-shot "infinite loop" anomaly when the pane keeps churning the
// same few states (see looksLikeLoop). Called only from the tick goroutine, on
// a real pane change, so the maps it touches need no locking. The flag clears
// once the loop signature disappears, so a later genuine loop re-fires.
func (p *Poller) trackLoop(ctx context.Context, s *store.Session, excerpt string) {
	h := append(p.paneHistory[s.ID], excerpt)
	if len(h) > loopWindow {
		h = h[len(h)-loopWindow:]
	}
	p.paneHistory[s.ID] = h

	if looksLikeLoop(h) {
		if !p.loopFlagged[s.ID] {
			p.loopFlagged[s.ID] = true
			p.raiseAnomaly(ctx, s, Anomaly{
				Kind:   anomalyLoop,
				Detail: "possible infinite loop — pane keeps churning the same output with no progress; consider interrupting the agent",
			})
		}
		return
	}
	p.loopFlagged[s.ID] = false
}

// publishApprovalEvent sends an event to the approval worker.
// Non-blocking: if the channel is full, the event is dropped (logged).
func (p *Poller) publishApprovalEvent(s *store.Session, pane string) {
	select {
	case p.ApprovalEvents <- ApprovalEvent{Session: s, Pane: pane}:
		// Event queued successfully
	default:
		// Channel full - drop event and log
		slog.Warn("poller: approval event dropped (channel full)", "agent", s.ID)
	}
}
