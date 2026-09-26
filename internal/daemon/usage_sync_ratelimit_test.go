package daemon

import (
	"context"
	"testing"
	"time"

	_ "github.com/srjn45/warden/internal/agentbackend/backends" // register adapters for RateLimitDetector checks
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

func TestLimitSessionsFromSnapshot_OpenCodeTransitions(t *testing.T) {
	ctx := context.Background()
	resetAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	sess := &store.Session{
		ID:      "oc-1",
		Backend: "opencode",
		Status:  store.StatusWorking,
	}
	st := &rateLimitStore{sessions: map[string]*store.Session{"oc-1": sess}}
	life := &fakeRateLimitLife{}
	// auto_resume off so OnTransition does not overwrite SetRateLimit with limitClearsAt.
	sched := NewRateLimitScheduler(life, st, 30*time.Minute, 6*time.Hour, time.Minute, false, "")
	var onLimitCalls int
	var gotUntil time.Time
	sched.OnLimit = func(_ *store.Session, until time.Time) {
		onLimitCalls++
		gotUntil = until
	}

	srv := &Server{store: st}
	srv.SetRateLimitScheduler(sched)

	snap := backendusage.Snapshot{
		Backends: []backendusage.BackendResult{{
			ID:     "opencode",
			Status: backendusage.StatusRateLimited,
			Usage: []backendusage.Limit{{
				ID:         "opencode:api",
				Scope:      "api",
				Label:      "API",
				ResetsAt:   &resetAt,
				LimitState: strPtr("rate_limited"),
			}},
		}},
	}
	srv.limitSessionsFromSnapshot(ctx, snap)

	got, err := st.Get(ctx, "oc-1")
	require.NoError(t, err)
	require.Equal(t, store.StatusRateLimited, got.Status, "pane-blind opencode session must transition")
	require.NotNil(t, got.RateLimitRestoreAt)
	require.True(t, got.RateLimitRestoreAt.Equal(resetAt), "reset time from snapshot must be persisted")
	require.Equal(t, 1, onLimitCalls, "OnTransition must fire after a successful CAS")
	_ = gotUntil // scheduleAt comes from limitClearsAt (pane-blind → retryInterval); restore is via SetRateLimit
}

func TestLimitSessionsFromSnapshot_ClaudeSkipped(t *testing.T) {
	ctx := context.Background()
	resetAt := time.Now().UTC().Add(time.Hour)
	sess := &store.Session{
		ID:      "claude-1",
		Backend: "claude",
		Status:  store.StatusWorking,
	}
	st := &rateLimitStore{sessions: map[string]*store.Session{"claude-1": sess}}
	sched := NewRateLimitScheduler(&fakeRateLimitLife{}, st, 30*time.Minute, 6*time.Hour, time.Minute, false, "")
	onLimitCalls := 0
	sched.OnLimit = func(*store.Session, time.Time) { onLimitCalls++ }

	srv := &Server{store: st}
	srv.SetRateLimitScheduler(sched)

	snap := backendusage.Snapshot{
		Backends: []backendusage.BackendResult{{
			ID:     "claude",
			Status: backendusage.StatusRateLimited,
			Usage: []backendusage.Limit{{
				ID:         "claude:session",
				Scope:      "session",
				Label:      "Session",
				ResetsAt:   &resetAt,
				LimitState: strPtr("rate_limited"),
			}},
		}},
	}
	srv.limitSessionsFromSnapshot(ctx, snap)

	got, err := st.Get(ctx, "claude-1")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, got.Status, "claude implements RateLimitDetector — usage poll must not double-trigger")
	require.Equal(t, 0, onLimitCalls)
	require.Equal(t, 0, st.updateStatusIfCalls)
}

func TestLimitSessionsFromSnapshot_AlreadyRateLimited(t *testing.T) {
	ctx := context.Background()
	sess := &store.Session{
		ID:      "oc-2",
		Backend: "opencode",
		Status:  store.StatusRateLimited,
	}
	st := &rateLimitStore{sessions: map[string]*store.Session{"oc-2": sess}}
	sched := NewRateLimitScheduler(&fakeRateLimitLife{}, st, 30*time.Minute, 6*time.Hour, time.Minute, false, "")
	onLimitCalls := 0
	sched.OnLimit = func(*store.Session, time.Time) { onLimitCalls++ }

	srv := &Server{store: st}
	srv.SetRateLimitScheduler(sched)

	snap := backendusage.Snapshot{
		Backends: []backendusage.BackendResult{{
			ID:     "opencode",
			Status: backendusage.StatusRateLimited,
			Usage: []backendusage.Limit{{
				ID:         "opencode:api",
				Scope:      "api",
				LimitState: strPtr("rate_limited"),
			}},
		}},
	}
	srv.limitSessionsFromSnapshot(ctx, snap)

	require.Equal(t, 0, onLimitCalls, "already-limited session must not re-fire OnTransition")
	require.Equal(t, 0, st.updateStatusIfCalls)
	require.Equal(t, 0, st.setRateLimitCalls)
}

func TestLimitSessionsFromSnapshot_NoLimitedBackends(t *testing.T) {
	ctx := context.Background()
	sess := &store.Session{
		ID:      "oc-3",
		Backend: "opencode",
		Status:  store.StatusWorking,
	}
	st := &rateLimitStore{sessions: map[string]*store.Session{"oc-3": sess}}
	sched := NewRateLimitScheduler(&fakeRateLimitLife{}, st, 30*time.Minute, 6*time.Hour, time.Minute, false, "")
	onLimitCalls := 0
	sched.OnLimit = func(*store.Session, time.Time) { onLimitCalls++ }

	srv := &Server{store: st}
	srv.SetRateLimitScheduler(sched)

	snap := backendusage.Snapshot{
		Backends: []backendusage.BackendResult{{
			ID:     "opencode",
			Status: backendusage.StatusOK,
			Usage:  []backendusage.Limit{{ID: "opencode:api", Scope: "api", Label: "API"}},
		}},
	}
	srv.limitSessionsFromSnapshot(ctx, snap)

	got, err := st.Get(ctx, "oc-3")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, got.Status)
	require.Equal(t, 0, onLimitCalls)
	require.Equal(t, 0, st.updateStatusIfCalls)
}
