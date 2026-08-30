package daemon

import (
	"context"
	"errors"
	"os"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
)

type pollerDeps struct {
	store store.Store
	run   lifecycle.Runner
	lc    *lifecycle.Lifecycle
}

// NewPollerDeps adapts the store + Runner + lifecycle to the poller's Deps.
func NewPollerDeps(st store.Store, run lifecycle.Runner, lc *lifecycle.Lifecycle) poller.Deps {
	return &pollerDeps{store: st, run: run, lc: lc}
}

func (d *pollerDeps) List(ctx context.Context) ([]*store.Session, error) { return d.store.List(ctx) }
func (d *pollerDeps) UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error) {
	return d.store.UpdateStatusIf(ctx, id, expected, next)
}
func (d *pollerDeps) UpdatePane(ctx context.Context, id, ex string) error {
	return d.store.Update(ctx, id, func(s *store.Session) error {
		s.LastPaneExcerpt = ex
		return nil
	})
}
func (d *pollerDeps) UpdateSubject(ctx context.Context, id, subject string) error {
	return d.store.Update(ctx, id, func(s *store.Session) error {
		s.Subject = subject
		return nil
	})
}
func (d *pollerDeps) Summarize(ctx context.Context, s *store.Session) (string, error) {
	return d.lc.Summarize(ctx, s)
}
func (d *pollerDeps) SessionAlive(ctx context.Context, name string) bool {
	_, err := d.run.Run(ctx, "", "tmux", "has-session", "-t", name)
	return err == nil
}
func (d *pollerDeps) CapturePane(ctx context.Context, name string) (string, error) {
	return d.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", name)
}
func (d *pollerDeps) ExitCode(_ context.Context, id string) (int, bool) {
	return d.lc.ReadExit(id)
}
func (d *pollerDeps) FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error) {
	swapped, err := d.store.FinalizeExit(ctx, id, expected, next, code)
	// The child just went terminal; its parent may now be a fully-terminal
	// tombstone ready to reap (agent sub-tree grouping, phase 3). Lazy primary
	// reap — the periodic sweep is the safety net.
	if swapped && err == nil {
		if s, gerr := d.store.Get(ctx, id); gerr == nil && s.ParentID != "" {
			reapTombstones(ctx, d.store, s.ParentID, d.SessionAlive)
		}
	}
	return swapped, err
}
func (d *pollerDeps) ClearExit(_ context.Context, id string) {
	d.lc.ClearExit(id)
}

// ContextTokens reads the agent's current context-window occupancy from its
// transcript JSONL. ok=false when the transcript is missing or has no model
// turn yet (a just-spawned agent).
func (d *pollerDeps) ContextTokens(_ context.Context, s *store.Session) (int, bool) {
	path := d.lc.TranscriptPath(s)
	if path == "" {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return ctxtokens.GetParser(s.Backend).LatestContextTokens(f)
}

// TranscriptUsage reads the agent's cumulative billed token usage (input+output
// summed over every assistant turn) from its transcript JSONL. ok=false when the
// transcript is missing or carries no usage yet — fail-open like ContextTokens,
// so spend tracking degrades to "unknown" rather than breaking the tick.
func (d *pollerDeps) TranscriptUsage(_ context.Context, s *store.Session) (int, int, bool) {
	path := d.lc.TranscriptPath(s)
	if path == "" {
		return 0, 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	u, ok := spend.GetParser(s.Backend).ParseUsage(f)
	return u.InputTokens, u.OutputTokens, ok
}

func (d *pollerDeps) UpdateContext(ctx context.Context, id string, tokens int, state string) error {
	return d.store.UpdateContext(ctx, id, tokens, state)
}

// Compact sends "/compact" to the agent's tmux pane via the bracketed-paste +
// Enter path. Called only when the agent is idle/waiting.
func (d *pollerDeps) Compact(ctx context.Context, s *store.Session) error {
	return d.lc.Input(ctx, s.ID, "/compact")
}

// Interrupt sends Escape to the agent's pane, cancelling the in-flight turn so a
// busy critical agent drops to idle and the force-compact path can /compact it.
func (d *pollerDeps) Interrupt(ctx context.Context, s *store.Session) error {
	return d.lc.SendKeys(ctx, s.ID, "Escape")
}

// Resume sends a prompt to a force-compacted agent so it picks its work back up
// after the interrupt + compaction (the bracketed-paste + Enter path).
func (d *pollerDeps) Resume(ctx context.Context, s *store.Session, prompt string) error {
	return d.lc.Input(ctx, s.ID, prompt)
}

func (d *pollerDeps) StampCompact(ctx context.Context, id string) error {
	return d.store.StampCompact(ctx, id)
}

func (d *pollerDeps) SendKeys(ctx context.Context, tmuxSession, keys string) error {
	return d.lc.SendKeys(ctx, tmuxSession, keys)
}

// RecordEvent appends a poller-raised health anomaly to the agent's record. A
// missing session is a soft no-op (the agent may have been deleted mid-tick).
func (d *pollerDeps) RecordEvent(ctx context.Context, id string, ev store.Event) error {
	err := d.store.AppendEvent(ctx, id, ev)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// ProjectsDir exposes the lifecycle's transcript root so the poller can pass it to
// a backend's DiscoverSessionID (same root TranscriptPath receives).
func (d *pollerDeps) ProjectsDir() string { return d.lc.ProjectsDir }

// SetSessionID persists a discovered agent-generated session id (discover-then-pin).
func (d *pollerDeps) SetSessionID(ctx context.Context, id, sessionID string) error {
	return d.store.SetSessionID(ctx, id, sessionID)
}
