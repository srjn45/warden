package cli

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestFastBrainCandidatesPolicyOrderAndRejections(t *testing.T) {
	st, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer st.Close()
	now := time.Now()
	for _, b := range []backendstore.Backend{
		{ID: "claude", Installed: true, Enabled: true, Tier: backendstore.TierSubscription, Default: true},
		{ID: "opencode", Installed: true, Enabled: true, Tier: backendstore.TierFree},
		{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierPayPerUse},
		{ID: "aider", Installed: true, Enabled: false, Tier: backendstore.TierFree},
		{ID: "goose", Installed: true, Enabled: true, Tier: backendstore.TierFree, LimitedUntil: now.Add(time.Hour)},
	} {
		require.NoError(t, st.Upsert(b))
	}
	src := newFastBrainCandidates(&lifecycle.Lifecycle{})
	src.SetStore(st)

	reasons := map[string]string{}
	var order []string
	for _, c := range src.Candidates(fastbrain.TierFast) {
		reasons[c.ID] = c.Ineligible
		order = append(order, c.ID)
	}
	require.Equal(t, "disabled", reasons["aider"])
	require.Equal(t, "paid_tier", reasons["codex"])
	require.Equal(t, "rate_limited", reasons["goose"])
	// Free tier precedes the (default) subscription backend on the fast tier.
	idx := func(id string) int {
		for i, o := range order {
			if o == id {
				return i
			}
		}
		return -1
	}
	require.Less(t, idx("opencode"), idx("claude"))

	// The thinking tier leads with the registry default.
	require.Equal(t, "claude", src.Candidates(fastbrain.TierThinking)[0].ID)
}

func TestFastBrainCandidatesFailOpenBeforeStore(t *testing.T) {
	src := newFastBrainCandidates(&lifecycle.Lifecycle{})
	cs := src.Candidates(fastbrain.TierFast)
	require.Len(t, cs, 1)
	require.Empty(t, cs[0].Ineligible)
}
