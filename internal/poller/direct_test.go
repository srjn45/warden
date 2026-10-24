package poller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

func rt(kind string) *store.SessionRuntime { return &store.SessionRuntime{Kind: kind} }

// Mixed fleet: the record's runtime identity decides who owns state. Direct
// agents are never classified, prompted or paned by the poller (even when the
// runtime reports no authority — a config rollback must not hand a live
// holder-backed agent back); legacy and explicit-tmux records are unchanged,
// even when a same-named direct session makes the runtime report authority.
func TestMixedRuntimeOwnership(t *testing.T) {
	d := &stubDeps{
		sessions: []*agentstore.Agent{
			{ID: "legacy", TmuxSession: "legacy", Status: store.StatusWorking},
			{ID: "tmux", TmuxSession: "tmux", Status: store.StatusWorking, Runtime: rt("tmux")},
			{ID: "direct", TmuxSession: "direct", Status: store.StatusWorking, Runtime: rt("direct")},
		},
		alive:       map[string]bool{"legacy": true, "tmux": true, "direct": true},
		panes:       map[string]string{"legacy": "❯ \n", "tmux": "❯ \n", "direct": "❯ \n"},
		updates:     map[string]store.Status{},
		paneUpdates: map[string]string{},
	}
	p := New(d, 5*time.Minute)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return fakeBackend{state: agentbackend.StateIdle} }
	p.Authoritative = func(key string) bool { return key == "tmux" } // collision: only for the tmux-kind record

	require.NoError(t, p.tick(context.Background()))

	require.Equal(t, store.StatusIdle, d.updates["legacy"])
	require.Equal(t, store.StatusIdle, d.updates["tmux"], "explicit tmux record is the poller's regardless of a same-named runtime session")
	require.NotContains(t, d.updates, "direct", "direct agent status is the classifier's")
	require.NotContains(t, d.paneUpdates, "direct", "no pane excerpt churn for a direct agent")
	require.Zero(t, d.sendKeysN, "the poller types nothing at a direct agent")
}

// The poller never races the classifier on a direct agent's dead/alive status
// either: a direct record whose tmux name is absent is NOT orphaned from here.
func TestDirectAgentNeverOrphanedByPoller(t *testing.T) {
	d := &stubDeps{
		sessions: []*agentstore.Agent{{ID: "direct", TmuxSession: "direct", Status: store.StatusWorking, Runtime: rt("direct")}},
		alive:    map[string]bool{},
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Empty(t, d.updates)
}

// Ancillary observation survives the guard: when the classifier has marked a
// direct agent rate_limited the scheduler still gets the banner, once per
// episode, without the poller touching status.
func TestDirectRateLimitObservationForwardedOncePerEpisode(t *testing.T) {
	banner := "You've hit your limit · resets 5pm"
	d := &stubDeps{
		sessions: []*agentstore.Agent{{ID: "direct", TmuxSession: "direct", Status: store.StatusRateLimited, Runtime: rt("direct")}},
		alive:    map[string]bool{"direct": true},
		panes:    map[string]string{"direct": banner},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	var obs []RateLimitObservation
	p.OnRateLimitObservation = func(o RateLimitObservation) { obs = append(obs, o) }

	require.NoError(t, p.tick(context.Background()))
	require.NoError(t, p.tick(context.Background()))
	require.Len(t, obs, 1, "one observation per limit episode")
	require.Contains(t, obs[0].FreshExcerpt, "hit your limit")
	require.Empty(t, d.updates, "status stays the classifier's")

	// Episode ends (classifier moved it on) and a new one starts: forwarded again.
	d.sessions[0].Status = store.StatusWorking
	require.NoError(t, p.tick(context.Background()))
	d.sessions[0].Status = store.StatusRateLimited
	require.NoError(t, p.tick(context.Background()))
	require.Len(t, obs, 2)
}
