package lifecycle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// countingHost is a pane host whose HasSession turns true once NewSession has
// run, with a deliberate yield between check and create to widen the window a
// check-then-act race needs.
type countingHost struct {
	mu      sync.Mutex
	live    map[string]bool
	creates atomic.Int32
	keys    atomic.Int32
	direct  bool
}

func (h *countingHost) NewSession(_ context.Context, name, _ string, _ ...string) error {
	h.creates.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live[name] = true
	return nil
}
func (h *countingHost) KillSession(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.live, name)
	return nil
}
func (h *countingHost) HasSession(_ context.Context, name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.live[name]
}
func (h *countingHost) CapturePane(context.Context, string) (string, error) { return "", nil }
func (h *countingHost) SendKeys(context.Context, string, ...string) error {
	h.keys.Add(1)
	return nil
}
func (h *countingHost) HostedDirectly(string) bool { return h.direct }

// F-LIFE-01: poller restore, operator restore and startup recovery racing on one
// terminal start exactly one shell; the losers get ErrAlreadyRunning.
func TestRestoreTerminalConcurrentLaunchesExactlyOnce(t *testing.T) {
	h := &countingHost{live: map[string]bool{}}
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.SetProcessHost(h)
	dir := t.TempDir()

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = lc.RestoreTerminal(context.Background(), "term-1", dir)
		}()
	}
	close(start)
	wg.Wait()

	require.EqualValues(t, 1, h.creates.Load(), "exactly one process may be launched")
	require.EqualValues(t, 1, h.keys.Load(), "the launch line is typed once")
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			require.ErrorIs(t, err, ErrAlreadyRunning)
		}
	}
	require.Equal(t, 1, ok)
}

// Different terminals are not serialized against each other.
func TestRestoreTerminalDistinctIDsBothLaunch(t *testing.T) {
	h := &countingHost{live: map[string]bool{}}
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.SetProcessHost(h)
	dir := t.TempDir()
	require.NoError(t, lc.RestoreTerminal(context.Background(), "a", dir))
	require.NoError(t, lc.RestoreTerminal(context.Background(), "b", dir))
	require.EqualValues(t, 2, h.creates.Load())
}

// A direct PTY session runs the user's shell as its process: no nested launch
// line is typed, so `exit` ends the session with its real status.
func TestRestoreTerminalDirectHostTypesNoLaunchLine(t *testing.T) {
	h := &countingHost{live: map[string]bool{}, direct: true}
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.SetProcessHost(h)
	require.NoError(t, lc.RestoreTerminal(context.Background(), "term-1", t.TempDir()))
	require.EqualValues(t, 1, h.creates.Load())
	require.EqualValues(t, 0, h.keys.Load())
}
