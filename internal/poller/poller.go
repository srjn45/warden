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
	// A visible prompt box ("❯ 1." / "Do you want", or a backend's own approval
	// marker) confirms waiting_for_input.
	if st == agentbackend.StateNeedsInput {
		return store.StatusWaitingForInput
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
		forceCompact:    map[string]fcState{},
		approveBreaker:  approval.NewBreaker(),
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

	// Never auto-confirm a destructive/irreversible action — escalate to a human.
	// This guard runs BEFORE Decide, so no allow rule can ever un-block it.
	if bad, marker := approval.IsDestructive(a); bad {
		slog.Warn("auto-approve BLOCKED: destructive action", "agent", s.ID, "marker", marker)
		return
	}
	// Evaluate against the allow/deny policy only when rules are configured. With
	// no rules the legacy on/off behavior stands (approve any recognized,
	// non-destructive prompt), keeping `auto_approve: true` backward-compatible.
	if pol.HasRules() {
		if d := pol.Decide(a); !d.Approve {
			slog.Debug("auto-approve skipped by policy", "agent", s.ID, "reason", d.Reason)
			return
		}
	}
	if a.AffirmativeIdx == 0 {
		slog.Debug("auto-approve skipped: no affirmative option", "agent", s.ID)
		return
	}
	if a.AffirmativeSticky && !pol.AllowSticky {
		slog.Debug("auto-approve skipped: only a sticky affirmative (allow_sticky off)", "agent", s.ID)
		return
	}

	// Circuit breaker: an identical prompt that keeps re-appearing after being
	// approved means approving is not unblocking the agent — stop feeding the
	// loop and escalate to a human. The prompt then sits unanswered, so the
	// agent surfaces as waiting_for_input.
	maxRepeats := pol.EffectiveMaxRepeats()
	sig := a.Action + "\x00" + a.Question
	if ok, trippedNow := p.approveBreaker.Allow(s.ID, sig, maxRepeats); !ok {
		if trippedNow {
			slog.Warn("auto-approve circuit breaker tripped", "agent", s.ID, "action", a.Action, "repeats", maxRepeats)
			p.raiseAnomaly(ctx, s, Anomaly{
				Kind: anomalyApprovalLoop,
				Detail: fmt.Sprintf("auto-approve halted: the identical prompt (%s) was approved %d times in a row without unblocking the agent — answer it manually or interrupt the agent",
					a.Action, maxRepeats),
			})
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

// tryLimitMenu auto-selects the "Stop and wait for limit to reset" choice on
// Claude's rate-limit menu (the safe option that parks the agent until the
// limit clears, vs. "Upgrade your plan"). It is a no-op unless the pane is that
// specific menu and rate_limit.auto_resume is on. Selecting the wait option
// dismisses the menu so the limit banner surfaces on a later tick, at which
// point the agent classifies as rate_limited and the daemon's scheduler drives
// the resume-after-clear. The numeric key is sent as a single keystroke, the
// same select-and-confirm mechanism tryAutoApprove uses for Claude's other
// numbered menus.
//
// TODO(open-question): confirm on a LIVE limit hit that the menu confirms on the
// bare number. Its footer reads "Enter to confirm", and the safe option is
// pre-highlighted (❯), so bare Enter is the fallback if numeric quick-select
// turns out not to apply here. This is the single place to change the keystroke,
// kept in sync with the daemon's resumeKey TODO.
func (p *Poller) tryLimitMenu(ctx context.Context, s *store.Session, pane string) {
	if !p.RateLimitAutoResume {
		return
	}
	idx, ok := LimitMenuOption(pane)
	if !ok {
		return
	}
	key := strconv.Itoa(idx)
	if err := p.deps.SendKeys(ctx, s.TmuxSession, key); err != nil {
		slog.Warn("rate-limit menu: failed to select wait option", "agent", s.ID, "err", err)
		return
	}
	slog.Info("rate-limit menu: auto-selected wait-for-reset", "agent", s.ID, "option", key)
	if p.OnChange != nil {
		p.OnChange()
	}
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
				}
			}
		}
		if alive && paneChanged && now.Sub(p.lastSummary[s.ID]) >= p.SummarizeAfter {
			p.dispatchSummary(ctx, s, now)
		}
		if p.TokenGuard && alive && p.CheckEvery >= 0 && now.Sub(p.lastCtxCheck[s.ID]) >= p.CheckEvery {
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
		len(p.forceCompact) == 0 && p.approveBreaker.Len() == 0 {
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
	for id := range p.forceCompact {
		if _, ok := live[id]; !ok {
			delete(p.forceCompact, id)
		}
	}
	p.approveBreaker.Prune(live)
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
