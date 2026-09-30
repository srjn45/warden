package daemon

import (
	"context"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/srjn45/warden/internal/tmuxproc"
)

// terminalPollerDeps adapts terminalstore + tmuxproc.Host to poller.TerminalDeps
// so status polling routes through terminalstore rather than the agent session
// store. Session projections are synthesized only for the watcher's callbacks.
type terminalPollerDeps struct {
	terms *terminalstore.Store
	host  tmuxproc.Host
	lc    *lifecycle.Lifecycle
}

// NewTerminalPollerDeps builds TerminalDeps backed by terminalstore.
func NewTerminalPollerDeps(terms *terminalstore.Store, host tmuxproc.Host, lc *lifecycle.Lifecycle) poller.TerminalDeps {
	return &terminalPollerDeps{terms: terms, host: host, lc: lc}
}

func (d *terminalPollerDeps) List(ctx context.Context) ([]*store.Session, error) {
	all, err := d.terms.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Session, 0, len(all))
	for _, t := range all {
		out = append(out, sessionFromTerminal(t))
	}
	return out, nil
}

func (d *terminalPollerDeps) SessionAlive(ctx context.Context, name string) bool {
	return d.host.HasSession(ctx, name)
}

func (d *terminalPollerDeps) CapturePane(ctx context.Context, name string) (string, error) {
	return d.host.CapturePane(ctx, name)
}

func (d *terminalPollerDeps) UpdateStatusIf(ctx context.Context, id string, from, to store.Status) (bool, error) {
	want := terminalStatusFromSession(from)
	next := terminalStatusFromSession(to)
	var swapped bool
	err := d.terms.Update(ctx, id, func(t *terminalstore.Terminal) error {
		if t.Status != want {
			return nil
		}
		t.Status = next
		t.UpdatedAt = time.Now().UTC()
		swapped = true
		return nil
	})
	return swapped, err
}

func (d *terminalPollerDeps) UpdatePane(ctx context.Context, id, excerpt string) error {
	// Terminal records do not carry pane excerpts (no AI surface). No-op so the
	// shared watcher tick still succeeds.
	_ = id
	_ = excerpt
	return nil
}

func (d *terminalPollerDeps) ExitCode(_ context.Context, id string) (int, bool) {
	if d.lc == nil {
		return 0, false
	}
	return d.lc.ReadExit(id)
}

func (d *terminalPollerDeps) ClearExit(_ context.Context, id string) {
	if d.lc != nil {
		d.lc.ClearExit(id)
	}
}

func (d *terminalPollerDeps) Restore(ctx context.Context, sess *store.Session) error {
	if d.lc == nil || sess == nil {
		return nil
	}
	return d.lc.RestoreTerminal(ctx, sess.ID, sess.Workdir)
}

var _ poller.TerminalDeps = (*terminalPollerDeps)(nil)
