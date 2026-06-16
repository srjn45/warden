package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeRateLimitLife embeds Lifecycle so unused methods stay nil.
type fakeRateLimitLife struct {
	Lifecycle
	restoreCalls int
	restoreErr   error
	inputCalls   int
	inputErr     error
}

func (f *fakeRateLimitLife) Restore(_ context.Context, sess *store.Session) error {
	f.restoreCalls++
	return f.restoreErr
}

func (f *fakeRateLimitLife) Input(_ context.Context, tmuxSession, text string) error {
	f.inputCalls++
	return f.inputErr
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

func (s *rateLimitStore) Get(_ context.Context, id string) (*store.Session, error) {
	if sess, ok := s.sessions[id]; ok {
		return sess, nil
	}
	return nil, store.ErrNotFound
}

func (s *rateLimitStore) List(_ context.Context) ([]*store.Session, error) {
	var sessions []*store.Session
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	return sessions, nil
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

func TestRateLimitScheduler_AttemptResume_Success(t *testing.T) {
	life := &fakeRateLimitLife{restoreErr: nil} // Success
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	st.sessions["test-123"] = &store.Session{
		ID:     "test-123",
		Status: store.StatusRateLimited,
	}

	sched := NewRateLimitScheduler(life, st)

	sched.attemptResume("test-123")

	// Verify Restore was called
	require.Equal(t, 1, life.restoreCalls)

	// Verify status updated to spawning
	require.Equal(t, 1, st.updateStatusIfCalls)

	// Verify ClearRateLimit called
	require.Equal(t, 1, st.clearRateLimitCalls)

	// Verify timer removed
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed after success")
}

func TestRateLimitScheduler_AttemptResume_SessionGone(t *testing.T) {
	life := &fakeRateLimitLife{}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	sched := NewRateLimitScheduler(life, st)

	// Session doesn't exist
	sched.attemptResume("nonexistent")

	// Should be no-op
	require.Equal(t, 0, life.restoreCalls)
}

func TestRateLimitScheduler_AttemptResume_StatusChanged(t *testing.T) {
	life := &fakeRateLimitLife{}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	// Session is no longer rate_limited
	st.sessions["test-123"] = &store.Session{
		ID:     "test-123",
		Status: store.StatusWorking,
	}

	sched := NewRateLimitScheduler(life, st)

	sched.attemptResume("test-123")

	// Should not attempt restore
	require.Equal(t, 0, life.restoreCalls)
}

func TestRateLimitScheduler_AttemptResume_StillLimited(t *testing.T) {
	life := &fakeRateLimitLife{
		restoreErr: errors.New("Rate limit. Try again later."),
	}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	st.sessions["test-123"] = &store.Session{
		ID:                  "test-123",
		Status:              store.StatusRateLimited,
		RateLimitRetryCount: 0,
	}

	sched := NewRateLimitScheduler(life, st)

	sched.attemptResume("test-123")

	// Verify SetRateLimit called (rescheduling)
	require.Equal(t, 1, st.setRateLimitCalls)

	// Verify event appended
	require.Equal(t, 1, st.appendEventCalls)

	// Timer should be rescheduled
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.True(t, exists, "timer should be rescheduled")
}

func TestRateLimitScheduler_AttemptResume_AlreadyRunning(t *testing.T) {
	life := &fakeRateLimitLife{
		restoreErr: lifecycle.ErrAlreadyRunning,
	}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	st.sessions["test-123"] = &store.Session{
		ID:          "test-123",
		Status:      store.StatusRateLimited,
		TmuxSession: "test-123",
	}

	sched := NewRateLimitScheduler(life, st)

	sched.attemptResume("test-123")

	// Should send resume prompt
	require.Equal(t, 1, life.inputCalls, "should send resume prompt via Input()")

	// Should be treated as success: status → spawning
	require.Equal(t, 1, st.updateStatusIfCalls, "status should be updated")

	// Should append event about sending prompt
	require.Equal(t, 1, st.appendEventCalls, "should append rate-limit-resumed event")

	// Timer should be removed
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed")
}

func TestRateLimitScheduler_AttemptResume_OtherError(t *testing.T) {
	life := &fakeRateLimitLife{
		restoreErr: errors.New("network connection failed"),
	}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	st.sessions["test-123"] = &store.Session{
		ID:     "test-123",
		Status: store.StatusRateLimited,
	}

	sched := NewRateLimitScheduler(life, st)

	sched.attemptResume("test-123")

	// Verify status updated to errored
	require.Equal(t, 1, st.updateStatusIfCalls)

	// Verify event appended with error detail
	require.Equal(t, 1, st.appendEventCalls)

	// Timer should be removed (not rescheduled)
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed after non-limit error")
}

func TestRateLimitScheduler_ReconstructTimers(t *testing.T) {
	life := &fakeRateLimitLife{}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	// Set up sessions: one rate-limited, one not
	futureTime := time.Now().Add(1 * time.Hour)
	st.sessions["limited-1"] = &store.Session{
		ID:                 "limited-1",
		Status:             store.StatusRateLimited,
		RateLimitRestoreAt: &futureTime,
	}
	st.sessions["working-1"] = &store.Session{
		ID:     "working-1",
		Status: store.StatusWorking,
	}

	sched := NewRateLimitScheduler(life, st)

	err := sched.ReconstructTimers(context.Background())
	require.NoError(t, err)

	// Verify timer created only for rate-limited session
	sched.mu.Lock()
	defer sched.mu.Unlock()

	_, exists := sched.timers["limited-1"]
	require.True(t, exists, "timer should exist for rate-limited session")

	_, exists = sched.timers["working-1"]
	require.False(t, exists, "should not create timer for non-rate-limited session")
}

func TestRateLimitScheduler_ReconstructTimers_PastTime(t *testing.T) {
	life := &fakeRateLimitLife{restoreErr: nil}
	st := &rateLimitStore{
		sessions: make(map[string]*store.Session),
	}

	// Restore time in the past
	pastTime := time.Now().Add(-1 * time.Hour)
	st.sessions["test-123"] = &store.Session{
		ID:                 "test-123",
		Status:             store.StatusRateLimited,
		RateLimitRestoreAt: &pastTime,
	}

	sched := NewRateLimitScheduler(life, st)

	err := sched.ReconstructTimers(context.Background())
	require.NoError(t, err)

	// Give timer a moment to fire (it should fire immediately)
	time.Sleep(50 * time.Millisecond)

	// Verify Restore was called (timer fired immediately)
	require.Equal(t, 1, life.restoreCalls)
}

func TestRateLimitScheduler_CancelTimer(t *testing.T) {
	sched := NewRateLimitScheduler(nil, nil)

	// Create a mock timer
	sched.timers["test-123"] = time.AfterFunc(1*time.Hour, func() {})

	sched.CancelTimer("test-123")

	sched.mu.Lock()
	defer sched.mu.Unlock()

	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed after cancel")
}

func TestRateLimitScheduler_CancelTimer_NotExists(t *testing.T) {
	sched := NewRateLimitScheduler(nil, nil)

	// Should not panic
	sched.CancelTimer("nonexistent")
}
