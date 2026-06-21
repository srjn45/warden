package poller

import (
	"context"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)

// classify derives a status from the latest pane capture + liveness.
// It only overrides the stored status when the pane (or staleness) gives a
// conclusive signal; otherwise it returns the existing status unchanged
// (hooks remain primary). sinceUpdate is how long the session has gone without
// any recorded activity (pane change, hook event, or status change).
func classify(s *store.Session, pane string, sessionAlive bool, sinceUpdate, stuckAfter time.Duration) store.Status {
	if !sessionAlive {
		return store.StatusOrphaned
	}

	// An agent that is actively streaming ("esc to interrupt") is working; a real
	// limit banner only appears once streaming has stopped, so working wins first
	// and we never even evaluate rate-limit detection on a live agent. This makes
	// a stray "rate limit" keyword in live output unable to misclassify it.
	if strings.Contains(pane, "esc to interrupt") {
		return store.StatusWorking
	}

	// Rate limit is checked before the waiting/idle heuristics so a banner is not
	// misread as waiting_for_input when its trailing prompt box is shown.
	if isLimited, _, _ := detectRateLimit(pane); isLimited {
		return store.StatusRateLimited
	}
	// A visible prompt box ("❯ 1." / "Do you want") confirms waiting_for_input.
	if strings.Contains(pane, "❯") || strings.Contains(pane, "Do you want") {
		return store.StatusWaitingForInput
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
	// UpdateContext persists the gauge (tokens + state band).
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	// Compact sends "/compact" to the agent (only called when it is idle/waiting).
	Compact(ctx context.Context, s *store.Session) error
	// StampCompact records that /compact was just sent (cooldown guard).
	StampCompact(ctx context.Context, id string) error
	// SendKeys sends a single key (e.g. numbered menu option) to the agent's tmux pane.
	SendKeys(ctx context.Context, tmuxSession, keys string) error
}

type Poller struct {
	deps           Deps
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

	// Context-size guard config + hooks (set by the daemon after New). When
	// TokenGuard is false the whole check is skipped. CompactCooldown bounds how
	// often /compact may be auto-sent to one agent.
	TokenGuard      bool
	TokenWarn       int
	TokenCrit       int
	WarnAlert       bool
	AutoCompact     bool
	CompactCooldown time.Duration
	CheckEvery      time.Duration // throttle for the per-agent transcript read
	// OnContextAlert, if set, fires once per upward threshold crossing.
	OnContextAlert func(sess *store.Session, state ctxtokens.State, tokens int)

	lastCtxCheck map[string]time.Time // last context read per session (tick goroutine only)

	// AutoApprovePolicy is the global allow/deny policy (from config). Per-session
	// Session.AutoApprove opts an agent into evaluation against this policy.
	AutoApprovePolicy approval.Policy

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

func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{
		deps:            d,
		stuckAfter:      stuckAfter,
		SummarizeAfter:  2 * time.Minute,
		lastSummary:     map[string]time.Time{},
		inflight:        map[string]struct{}{},
		lastCtxCheck:    map[string]time.Time{},
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

// tryAutoApprove attempts to auto-approve a recognized prompt by pressing its
// least-privilege affirmative ("yes") option. Only attempts auto-approval if:
//   - AutoApprovePolicy.Enabled OR session.AutoApprove is true (the participate-gate)
//   - The pane content parses as a recognized prompt (approval.Parse ok=true)
//
// A recognized prompt naming a destructive/irreversible action is blocked
// unconditionally (the destructive guard runs first and is not configurable).
// The prompt must then match the allow/deny policy (AutoApprovePolicy.Decide):
// deny wins over allow, and an empty allow list approves nothing. Prompts with no
// affirmative option are skipped, as are sticky-only "don't ask again"
// affirmatives unless AutoApprovePolicy.AllowSticky is set.
//
// Idempotent and safe to call repeatedly on the same prompt: an unrecognized or
// already-dismissed prompt is a logged no-op.
func (p *Poller) tryAutoApprove(ctx context.Context, s *store.Session, pane string) {
	// participate-gate: global master switch OR per-session opt-in.
	if !p.AutoApprovePolicy.Enabled && !s.AutoApprove {
		return
	}

	// Parse the approval
	a, ok := approval.Parse(pane)
	if !ok || len(a.Options) == 0 {
		log.Printf("auto-approve skipped for %s: unrecognized prompt", s.ID)
		return
	}

	// Never auto-confirm a destructive/irreversible action — escalate to a human.
	// This guard runs BEFORE Decide, so no allow rule can ever un-block it.
	if bad, marker := approval.IsDestructive(a); bad {
		log.Printf("auto-approve BLOCKED for %s: destructive (%q)", s.ID, marker)
		return
	}
	// Evaluate against the allow/deny policy (deny wins; empty allow approves nothing).
	if d := p.AutoApprovePolicy.Decide(a); !d.Approve {
		log.Printf("auto-approve skipped for %s: %s", s.ID, d.Reason)
		return
	}
	if a.AffirmativeIdx == 0 {
		log.Printf("auto-approve skipped for %s: no affirmative option", s.ID)
		return
	}
	if a.AffirmativeSticky && !p.AutoApprovePolicy.AllowSticky {
		log.Printf("auto-approve skipped for %s: only a sticky affirmative (allow_sticky off)", s.ID)
		return
	}

	key := strconv.Itoa(a.AffirmativeIdx)
	if err := p.deps.SendKeys(ctx, s.TmuxSession, key); err != nil {
		log.Printf("auto-approve failed for %s: %v", s.ID, err)
		return
	}

	log.Printf("auto-approved %s -> option %s: %s", s.ID, key, a.Options[a.AffirmativeIdx-1])
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
			// For errored agents: if the tmux session is still alive the error was
			// transient (e.g. a rate-limit resume race) — fall through to reclassify
			// so the TUI reflects the real state. done/orphaned are always skipped.
			if s.Status != store.StatusErrored || !p.deps.SessionAlive(ctx, s.TmuxSession) {
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
				log.Printf("poller: finalize %s: %v", s.ID, err)
				continue // leave the file; retry next tick
			}
			p.deps.ClearExit(ctx, s.ID) // consumed (clear even if CAS lost — the file is stale)
			if swapped {
				changed = true
				if p.OnTransition != nil {
					p.OnTransition(s, s.Status, next)
				}
			}
			continue
		}
		alive := p.deps.SessionAlive(ctx, s.TmuxSession)
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
			next := classify(s, pane, alive, time.Since(s.UpdatedAt), p.stuckAfter)
			if next != s.Status {
				// CAS on the snapshot's status: if a hook changed it since List,
				// the swap is skipped and the hook's newer status stands.
				if ok, err := p.deps.UpdateStatusIf(ctx, s.ID, s.Status, next); err != nil {
					log.Printf("poller: update %s: %v", s.ID, err)
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
	if len(p.lastSummary) == 0 && len(p.lastCtxCheck) == 0 {
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
		log.Printf("poller: summarize %s: %v", s.ID, err)
		return
	}
	if subj == "" || subj == s.Subject {
		return
	}
	if err := p.deps.UpdateSubject(ctx, s.ID, subj); err != nil {
		log.Printf("poller: subject %s: %v", s.ID, err)
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
				log.Printf("poller tick: %v", err)
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

// publishApprovalEvent sends an event to the approval worker.
// Non-blocking: if the channel is full, the event is dropped (logged).
func (p *Poller) publishApprovalEvent(s *store.Session, pane string) {
	select {
	case p.ApprovalEvents <- ApprovalEvent{Session: s, Pane: pane}:
		// Event queued successfully
	default:
		// Channel full - drop event and log
		log.Printf("poller: approval event dropped for %s (channel full)", s.ID)
	}
}
