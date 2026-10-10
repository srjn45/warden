package updater

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time                           { return c.now }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) { c.now = c.now.Add(d) }

// fakeSvc is a scriptable ServiceController.
type fakeSvc struct {
	t         *testing.T
	failOnUse bool

	mu       sync.Mutex
	restarts int
	stops    int
	state    ServiceState
	diag     string
	// restartErr/exited are consulted with the current restart count.
	restartErr func(n int) error
	stopErr    func() error
	exited     func(n int) bool
}

func (s *fakeSvc) use() {
	if s.failOnUse {
		s.t.Fatal("service must not be touched")
	}
}

// count is the number of restarts so far (safe for probes to read).
func (s *fakeSvc) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.restarts }

func (s *fakeSvc) State(context.Context) ServiceState {
	s.use()
	if s.state.Kind == "" {
		return ServiceState{Kind: ServiceSystemd, Active: true}
	}
	return s.state
}

func (s *fakeSvc) Restart(context.Context) error {
	s.use()
	s.mu.Lock()
	s.restarts++
	n := s.restarts
	s.mu.Unlock()
	if s.restartErr != nil {
		return s.restartErr(n)
	}
	return nil
}

func (s *fakeSvc) Stop(context.Context) error {
	s.use()
	s.mu.Lock()
	s.stops++
	s.mu.Unlock()
	if s.stopErr != nil {
		return s.stopErr()
	}
	return nil
}

func (s *fakeSvc) Exited(context.Context) bool {
	if s.exited == nil {
		return false
	}
	return s.exited(s.count())
}

func (s *fakeSvc) Diagnostics(context.Context) string { return s.diag }

type fakeProbe struct {
	fn    func() (Health, error)
	calls int
}

func (p *fakeProbe) Probe(context.Context) (Health, error) {
	p.calls++
	if p.fn == nil {
		return Health{}, errors.New("connection refused")
	}
	return p.fn()
}

// versionByRestarts: v1 until the first restart, then v2 (v1 again after a
// rollback restart when rollbackOK).
func versionByRestarts(s *fakeSvc, before, after string) *fakeProbe {
	return &fakeProbe{fn: func() (Health, error) {
		if s.count() == 0 {
			return Health{Status: "ok", Version: before}, nil
		}
		return Health{Status: "ok", Version: after}, nil
	}}
}
