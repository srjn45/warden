package daemon

import (
	"context"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/poller"
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
func (d *pollerDeps) ExitCode(_ context.Context, id string) (int, bool) {
	return d.lc.ReadExit(id)
}
func (d *pollerDeps) FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error) {
	return d.store.FinalizeExit(ctx, id, expected, next, code)
}
func (d *pollerDeps) ClearExit(_ context.Context, id string) {
	d.lc.ClearExit(id)
}
