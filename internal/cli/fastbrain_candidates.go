package cli

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/lifecycle"
)

// fastBrainCandidates builds Fast-Brain runner candidates from the backend
// registry. Candidates carry only bounded labels (backend id, cost tier) and a
// bounded rejection reason; ordering follows policy: cheapest eligible first,
// the registry default breaking ties (and leading for the thinking tier).
// Paid pay-per-use and unclassified backends are never eligible.
type fastBrainCandidates struct {
	lc    *lifecycle.Lifecycle
	store atomic.Pointer[backendstore.Store]
	now   func() time.Time
}

func newFastBrainCandidates(lc *lifecycle.Lifecycle) *fastBrainCandidates {
	return &fastBrainCandidates{lc: lc, now: time.Now}
}

// SetStore supplies the registry once the daemon has opened it. Until then the
// lifecycle's own configured headless backend serves as the single candidate.
func (c *fastBrainCandidates) SetStore(s *backendstore.Store) { c.store.Store(s) }

func costRank(tier string) int {
	switch tier {
	case backendstore.TierFree:
		return 0
	case backendstore.TierSubscription:
		return 1
	}
	return 2
}

// Candidates implements fastbrain.CandidateSource.
func (c *fastBrainCandidates) Candidates(tier fastbrain.Tier) []fastbrain.Candidate {
	st := c.store.Load()
	if st == nil {
		return []fastbrain.Candidate{{ID: "default", Provider: "default", CostTier: "unknown", Runner: fastbrain.RunnerFunc(c.lc.RunClaudeP)}}
	}
	rows, err := st.List()
	if err != nil || len(rows) == 0 {
		// Registry unreadable: fail open to the lifecycle's configured backend
		// rather than disabling internal decisions.
		return []fastbrain.Candidate{{ID: "default", Provider: "default", CostTier: "unknown", Runner: fastbrain.RunnerFunc(c.lc.RunClaudeP)}}
	}
	now := c.now()
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if tier == fastbrain.TierThinking && a.Default != b.Default {
			return a.Default
		}
		if ra, rb := costRank(a.Tier), costRank(b.Tier); ra != rb {
			return ra < rb
		}
		if a.Default != b.Default {
			return a.Default
		}
		return a.ID < b.ID
	})
	out := make([]fastbrain.Candidate, 0, len(rows))
	for _, b := range rows {
		if b.ID == "terminal" {
			continue
		}
		id := b.ID
		cand := fastbrain.Candidate{ID: id, Provider: id, CostTier: b.Tier}
		switch {
		case !b.Enabled:
			cand.Ineligible = "disabled"
		case b.IsLocal:
			cand.Ineligible = "no_headless"
		case !b.Installed:
			cand.Ineligible = "not_installed"
		case b.Tier != backendstore.TierFree && b.Tier != backendstore.TierSubscription:
			cand.Ineligible = "paid_tier"
		case b.LimitedUntil.After(now):
			cand.Ineligible = "rate_limited"
		case !c.lc.HasHeadless(id):
			cand.Ineligible = "no_headless"
		default:
			cand.Runner = fastbrain.RunnerFunc(func(ctx context.Context, prompt string) (string, error) {
				return c.lc.RunHeadless(ctx, id, prompt)
			})
		}
		out = append(out, cand)
	}
	return out
}
