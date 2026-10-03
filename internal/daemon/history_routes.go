package daemon

import (
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

// filterClosed narrows archived records to those updated at/after `since` (zero
// time = no lower bound) and matching `typ` (empty = any type), preserving the
// newest-first order ListClosed already guarantees. A positive limit caps the
// result. Terminal-kind sessions are dropped — history is an AI-agent record and
// a shell has no work to report. Pure: it never mutates the input slice.
func filterClosed(sessions []*agentstore.Agent, since time.Time, typ store.Type, role string, limit int) []*agentstore.Agent {
	out := make([]*agentstore.Agent, 0, len(sessions))
	for _, s := range sessions {
		if !since.IsZero() && s.UpdatedAt.Before(since) {
			continue
		}
		if role != "" {
			if effectiveSessionRole(s.Role, s.Type) != role {
				continue
			}
		} else if typ != "" && s.Type != typ {
			// Legacy type-only filter when the type did not map onto a role.
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
