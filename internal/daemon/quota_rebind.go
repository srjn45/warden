package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

// quotaRebindEvent is the agent event recorded when the quota binding is moved to
// the bucket of the model the backend is really running.
const quotaRebindEvent = "quota_rebound"

// NewQuotaRebinder returns the poller.OnObservedQuotaScope handler: when the pane
// shows the agent running a model whose quota scope differs from its binding, it
// re-binds the persisted QuotaBinding to the observed bucket (so usage
// reconciliation watches the bucket that is actually draining) and records an
// event on the agent. The binding is re-checked under the store's update lock, so
// a concurrent swap/restore that already changed it wins.
func NewQuotaRebinder(st agentstore.AgentStore) func(*agentstore.Agent, string, string) {
	return func(s *agentstore.Agent, model, scope string) {
		if s == nil {
			return
		}
		ctx := context.Background()
		var detail string
		err := st.Update(ctx, s.ID, func(a *agentstore.Agent) error {
			nb, changed := a.QuotaBinding.Rebound(scope)
			if !changed {
				return nil
			}
			detail = fmt.Sprintf("requested route=%s bucket=%v; running %q bucket=%s",
				a.QuotaBinding.Domain.Route, a.QuotaBinding.MandatoryBuckets, model, scope)
			a.QuotaBinding = nb
			return nil
		})
		if err != nil {
			slog.Warn("quota rebind failed", "agent", s.ID, "err", err)
			return
		}
		if detail == "" {
			return
		}
		if err := st.AppendEvent(ctx, s.ID, store.Event{TS: time.Now().UTC(), Type: quotaRebindEvent, Detail: detail}); err != nil {
			slog.Warn("quota rebind event failed", "agent", s.ID, "err", err)
		}
	}
}
