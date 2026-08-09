package daemon

import (
	"time"

	"github.com/srjn45/warden/internal/store"
)

// filterClosed narrows archived records to those updated at/after `since` (zero
// time = no lower bound) and matching `typ` (empty = any type), preserving the
// newest-first order ListClosed already guarantees. A positive limit caps the
// result. Terminal-kind sessions are dropped — history is an AI-agent record and
// a shell has no work to report. Pure: it never mutates the input slice.
func filterClosed(sessions []*store.Session, since time.Time, typ store.Type, limit int) []*store.Session {
	out := make([]*store.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.IsTerminal() {
			continue
		}
		if !since.IsZero() && s.UpdatedAt.Before(since) {
			continue
		}
		if typ != "" && s.Type != typ {
			continue
		}
		out = append(out, s)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
