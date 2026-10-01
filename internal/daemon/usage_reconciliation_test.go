package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/stretchr/testify/require"
)

// reconciliationClock is deliberately movable: poll decisions are based on
// injected observations, not wall-clock sleeps, so cadence tests stay instant.
type reconciliationClock struct{ now time.Time }

func (c *reconciliationClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type reconciliationRegistry struct{ rows []backendstore.Backend }

func (r reconciliationRegistry) List() ([]backendstore.Backend, error) { return r.rows, nil }

type reconciliationAdapter struct {
	id      string
	status  backendusage.Status
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (a *reconciliationAdapter) BackendID() string { return a.id }
func (a *reconciliationAdapter) Fetch(context.Context, backendstore.Backend) backendusage.Result {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.started != nil {
		a.started <- struct{}{}
	}
	if a.release != nil {
		<-a.release
	}
	return backendusage.Result{Status: a.status, ObservedAt: time.Now().UTC()}
}
func (a *reconciliationAdapter) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

func reconciliationServer(enabled bool, adapters ...backendusage.Adapter) *Server {
	rows := make([]backendstore.Backend, len(adapters))
	for i, a := range adapters {
		rows[i] = backendstore.Backend{ID: a.BackendID(), Tier: backendstore.TierSubscription, Enabled: true, Installed: true}
	}
	s := NewServer(nil, nil, nil, 0, false, nil, nil, nil)
	s.SetUsageService(backendusage.NewService(reconciliationRegistry{rows: rows}, adapters...))
	s.SetUsageReconciliation(enabled, time.Minute, 15*time.Minute)
	return s
}

func TestUsageReconciliationDisabledMakesNoProviderCalls(t *testing.T) {
	a := &reconciliationAdapter{id: "codex", status: backendusage.StatusOK}
	s := reconciliationServer(false, a)
	require.False(t, s.usageReconciliationOnce(context.Background(), time.Minute))
	require.Zero(t, a.count())
}

func TestUsageReconciliationCadenceAndBackoffWithFakeClock(t *testing.T) {
	clock := &reconciliationClock{now: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	a := &reconciliationAdapter{id: "codex", status: backendusage.StatusError}
	s := reconciliationServer(true, a)
	require.True(t, s.usageReconciliationOnce(context.Background(), 15*time.Minute))
	require.Equal(t, 1, a.count())
	clock.Advance(time.Minute) // first retry cadence
	require.True(t, s.usageReconciliationOnce(context.Background(), 15*time.Minute))
	require.Equal(t, 2, a.count())
	clock.Advance(2 * time.Minute) // exponential backoff decision
	require.Equal(t, 2*time.Minute, reconciliationNextDelay(time.Minute, time.Minute, true))
	require.Equal(t, 8*time.Minute, reconciliationNextDelay(time.Minute, 8*time.Minute, true))
	require.Equal(t, time.Minute, reconciliationNextDelay(time.Minute, 8*time.Minute, false))
}

func TestUsageReconciliationPreventsOverlappingPolls(t *testing.T) {
	a := &reconciliationAdapter{id: "codex", status: backendusage.StatusOK, started: make(chan struct{}, 1), release: make(chan struct{})}
	s := reconciliationServer(true, a)
	done := make(chan bool, 1)
	go func() { done <- s.usageReconciliationOnce(context.Background(), time.Minute) }()
	<-a.started
	require.False(t, s.usageReconciliationOnce(context.Background(), time.Minute))
	require.Equal(t, 1, a.count())
	close(a.release)
	<-done
}

func TestUsageReconciliationRestartsAndPublishes(t *testing.T) {
	a := &reconciliationAdapter{id: "codex", status: backendusage.StatusOK}
	s := reconciliationServer(true, a)
	ch, unsub := s.hub.subscribe()
	defer unsub()
	s.usageReconciliationOnce(context.Background(), time.Minute)
	select {
	case <-ch:
	default:
		t.Fatal("snapshot change was not published")
	}
	// A replacement daemon starts with a clean in-flight guard and polls again.
	restarted := reconciliationServer(true, a)
	restarted.usageReconciliationOnce(context.Background(), time.Minute)
	require.Equal(t, 2, a.count())
}

func TestUsageReconciliationIsolatesProviders(t *testing.T) {
	failing := &reconciliationAdapter{id: "codex", status: backendusage.StatusTimeout}
	healthy := &reconciliationAdapter{id: "claude", status: backendusage.StatusOK}
	s := reconciliationServer(true, failing, healthy)
	require.True(t, s.usageReconciliationOnce(context.Background(), time.Minute))
	require.Equal(t, 1, failing.count())
	require.Equal(t, 1, healthy.count(), "one provider failure must not suppress another account/provider")
}
