package daemon

import (
	"context"

	"github.com/srajanpathak/warden/internal/lifecycle"
	"github.com/srajanpathak/warden/internal/poller"
	"github.com/srajanpathak/warden/internal/store"
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
	return d.store.UpdatePane(ctx, id, ex)
}
func (d *pollerDeps) UpdateSubject(ctx context.Context, id, subject string) error {
	return d.store.UpdateSubject(ctx, id, subject)
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
