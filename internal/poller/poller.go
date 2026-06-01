package poller

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
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
	UpdateStatus(ctx context.Context, id string, st store.Status) error
	UpdatePane(ctx context.Context, id, excerpt string) error
	SessionAlive(ctx context.Context, tmuxName string) bool
	CapturePane(ctx context.Context, tmuxName string) (string, error)
}

type Poller struct {
	deps       Deps
	stuckAfter time.Duration
}

func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{deps: d, stuckAfter: stuckAfter}
}

func isTerminal(s store.Status) bool {
	return s == store.StatusDone
}

func (p *Poller) tick(ctx context.Context) error {
	sessions, err := p.deps.List(ctx)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if isTerminal(s.Status) {
			continue
		}
		alive := p.deps.SessionAlive(ctx, s.TmuxSession)
		var pane string
		if alive {
			pane, _ = p.deps.CapturePane(ctx, s.TmuxSession)
			// Persist the excerpt ONLY when it actually changed. This keeps
			// updated_at stable while a session is silent, which is exactly how
			// staleness (stuck detection) is measured — and avoids a needless
			// Mongo write every tick.
			if excerpt := lastLines(pane, 20); excerpt != s.LastPaneExcerpt {
				_ = p.deps.UpdatePane(ctx, s.ID, excerpt)
			}
		}
		next := classify(s, pane, alive, time.Since(s.UpdatedAt), p.stuckAfter)
		if next != s.Status {
			if err := p.deps.UpdateStatus(ctx, s.ID, next); err != nil {
				log.Printf("poller: update %s: %v", s.ID, err)
			}
		}
	}
	return nil
}

// Run ticks every interval until ctx is cancelled.
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
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
