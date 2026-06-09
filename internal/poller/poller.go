package poller

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

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
	if strings.Contains(pane, "esc to interrupt") {
		return store.StatusWorking
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

	// Summarization runs `claude -p`, which is slow, so it is dispatched to
	// background workers rather than blocking the tick loop. mu guards inflight;
	// wg tracks live workers so Run can drain them on shutdown.
	mu       sync.Mutex
	inflight map[string]struct{} // session ids with a summarizer currently running
	wg       sync.WaitGroup
}

func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{
		deps:           d,
		stuckAfter:     stuckAfter,
		SummarizeAfter: 2 * time.Minute,
		lastSummary:    map[string]time.Time{},
		inflight:       map[string]struct{}{},
	}
}

// summaryTimeout bounds a single `claude -p` summary call. Without it, a hung
// model call holds the session's inflight flag indefinitely (it is cleared only
// when runSummary returns), permanently suppressing that session's subject
// refreshes. Var (not const) so tests can shrink it.
var summaryTimeout = 60 * time.Second

func isTerminal(s store.Status) bool {
	return s == store.StatusDone
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
				}
			}
		}
		if alive && paneChanged && now.Sub(p.lastSummary[s.ID]) >= p.SummarizeAfter {
			p.dispatchSummary(ctx, s, now)
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
	if len(p.lastSummary) == 0 {
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
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain in-flight summarizers; ctx cancellation already aborts their
			// `claude -p` subprocesses, so this returns promptly.
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
