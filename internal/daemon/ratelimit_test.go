package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeRateLimitLife embeds Lifecycle so unused methods stay nil.
type fakeRateLimitLife struct {
	Lifecycle
	restoreCalls int
	restoreErr   error
}

func (f *fakeRateLimitLife) Restore(_ context.Context, sess *store.Session) error {
	f.restoreCalls++
	return f.restoreErr
}

// rateLimitStore is a minimal store fake for RateLimitScheduler tests.
type rateLimitStore struct {
	store.Store
	setRateLimitCalls   int
	clearRateLimitCalls int
	updateStatusIfCalls int
	appendEventCalls    int
	sessions            map[string]*store.Session
}

func (s *rateLimitStore) SetRateLimit(_ context.Context, id string, restoreAt time.Time, retryCount int) error {
	s.setRateLimitCalls++
	if sess, ok := s.sessions[id]; ok {
		sess.RateLimitRestoreAt = &restoreAt
		sess.RateLimitRetryCount = retryCount
	}
	return nil
}

func (s *rateLimitStore) ClearRateLimit(_ context.Context, id string) error {
	s.clearRateLimitCalls++
	return nil
}

func (s *rateLimitStore) UpdateStatusIf(_ context.Context, id string, expected, next store.Status) (bool, error) {
	s.updateStatusIfCalls++
	if sess, ok := s.sessions[id]; ok {
		if sess.Status == expected {
			sess.Status = next
			return true, nil
		}
	}
	return false, nil
}

func (s *rateLimitStore) AppendEvent(_ context.Context, id string, ev store.Event) error {
	s.appendEventCalls++
	return nil
}

func TestRateLimitScheduler_OnTransition(t *testing.T) {
	life := &fakeRateLimitLife{}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	sched := NewRateLimitScheduler(life, st)

	sess := &store.Session{
		ID:              "test-123",
		Status:          store.StatusRateLimited,
		LastPaneExcerpt: "Rate limit exceeded. Try again later.",
	}
	st.sessions["test-123"] = sess

	// Trigger transition
	sched.OnTransition(sess, store.StatusWorking, store.StatusRateLimited)

	// Verify SetRateLimit was called
	require.Equal(t, 1, st.setRateLimitCalls, "SetRateLimit should be called")

	// Verify timer was created
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.True(t, exists, "timer should be created for session")
}

func TestRateLimitScheduler_OnTransition_IgnoresOtherStatuses(t *testing.T) {
	life := &fakeRateLimitLife{}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	sched := NewRateLimitScheduler(life, st)

	sess := &store.Session{ID: "test-123"}

	// Transition to non-rate-limited status
	sched.OnTransition(sess, store.StatusWorking, store.StatusIdle)

	// Verify no action taken
	require.Equal(t, 0, st.setRateLimitCalls)

	sched.mu.Lock()
	defer sched.mu.Unlock()
	require.Empty(t, sched.timers)
}
