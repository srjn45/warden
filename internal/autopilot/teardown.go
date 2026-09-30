package autopilot

import (
	"context"
	"strings"
)

// TeardownLive stops a plan-bound Autopilot executor and deletes its live
// Autopilot store row. Idempotent: missing IDs succeed. In-flight workers are
// not terminated here — the daemon's plan finalization cleanup owns worker
// teardown so Plan events / worktree evidence stay consistent.
func (c *Controller) TeardownLive(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if r, ok := c.runs[id]; ok {
		if r.state != StateStopped && r.state != StateComplete && r.state != StateDisabled {
			c.stopRunLocked(ctx, r)
			r.state = StateStopped
			c.persistRunLocked(r)
		}
		if c.claims != nil {
			c.claims.release(id)
		}
		delete(c.runs, id)
	}
	if c.store != nil {
		_ = c.store.Delete(id) // best-effort; plan-bound runs may lack a RunRecord
	}
	if c.live != nil {
		if err := c.live.Delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
