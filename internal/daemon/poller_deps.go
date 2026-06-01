package daemon

import (
	"context"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/poller"
	"github.com/srajanpathak/agentctl/internal/store"
)

type pollerDeps struct {
	store store.Store
	run   lifecycle.Runner
}

// NewPollerDeps adapts the store + a Runner to the poller's Deps interface.
func NewPollerDeps(st store.Store, run lifecycle.Runner) poller.Deps {
	return &pollerDeps{store: st, run: run}
}

func (d *pollerDeps) List(ctx context.Context) ([]*store.Session, error) { return d.store.List(ctx) }
func (d *pollerDeps) UpdateStatus(ctx context.Context, id string, st store.Status) error {
	return d.store.UpdateStatus(ctx, id, st)
}
func (d *pollerDeps) UpdatePane(ctx context.Context, id, ex string) error {
	return d.store.UpdatePane(ctx, id, ex)
}
func (d *pollerDeps) SessionAlive(ctx context.Context, name string) bool {
	_, err := d.run.Run(ctx, "", "tmux", "has-session", "-t", name)
	return err == nil
}
func (d *pollerDeps) CapturePane(ctx context.Context, name string) (string, error) {
	return d.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", name)
}
