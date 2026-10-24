package poller

import (
	"context"
	"log/slog"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

// directOwned reports whether the poller must stay out of s's state, approval
// and input authority because a directly hosted PTY agent owns them. The
// durable record decides: Runtime.Kind == "direct" is authoritative (a config
// rollback must not hand a live holder-backed agent back to the poller, and a
// same-named legacy pane must never be classified as one). Only a record with
// no runtime identity falls back to the Authoritative callback; an explicit
// "tmux" record is always the poller's — legacy sessions are unchanged.
func (p *Poller) directOwned(s *agentstore.Agent) bool {
	if s.Runtime != nil && s.Runtime.Kind != "" {
		return s.Runtime.Kind == "direct"
	}
	return p.Authoritative != nil && p.Authoritative(s.RuntimeKey())
}

// observeDirect runs the ancillary observations a direct agent still needs
// from the poller tick, none of which touch status, approvals or input:
//
//   - provider session-id discovery (resume/transcript keying);
//   - the observed quota scope (rebind detection);
//   - the rate-limit observation for the scheduler, fired once per limit
//     episode when the classifier has already marked the agent rate_limited
//     (the classifier owns the status; the scheduler still needs the banner).
//
// Capture failures are transient and skipped; nothing here holds a global lock.
func (p *Poller) observeDirect(ctx context.Context, s *agentstore.Agent) {
	key := s.RuntimeKey()
	if s.Status != store.StatusRateLimited {
		delete(p.directLimit, s.ID)
	}
	if !p.deps.SessionAlive(ctx, key) {
		return
	}
	p.discoverSessionID(ctx, s)
	needScope := p.OnObservedQuotaScope != nil && s.QuotaBinding != nil
	needLimit := p.OnRateLimitObservation != nil && s.Status == store.StatusRateLimited && !p.directLimit[s.ID]
	if !needScope && !needLimit {
		return
	}
	pane, err := p.deps.CapturePane(ctx, key)
	if err != nil {
		return
	}
	if needScope {
		if qo, ok := p.backendFor(s).(agentbackend.QuotaScopeObserver); ok {
			if model, scope, ok := qo.ObservedQuotaScope(pane); ok {
				if _, differs := s.QuotaBinding.Rebound(scope); differs {
					p.OnObservedQuotaScope(s, model, scope)
				}
			}
		}
	}
	if needLimit {
		if p.directLimit == nil {
			p.directLimit = map[string]bool{}
		}
		p.directLimit[s.ID] = true
		slog.Info("poller: direct agent rate-limited; forwarding observation", "agent", s.ID)
		p.OnRateLimitObservation(NewRateLimitObservation(s.ID, lastLines(pane, 20)))
	}
}
