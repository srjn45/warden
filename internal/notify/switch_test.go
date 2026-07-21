package notify

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// recNotifier records the titles it was asked to deliver.
type recNotifier struct {
	mu     sync.Mutex
	titles []string
}

func (r *recNotifier) Notify(title, _ string) {
	r.mu.Lock()
	r.titles = append(r.titles, title)
	r.mu.Unlock()
}

func (r *recNotifier) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.titles...)
}

// TestSwitchSwapsBacking proves the Switch forwards to its current backing
// notifier and that Set atomically redirects delivery — the seam a config reload
// of notify.* uses to rebuild the delivery chain without re-wiring hooks.
func TestSwitchSwapsBacking(t *testing.T) {
	a, b := &recNotifier{}, &recNotifier{}
	sw := NewSwitch(a)

	sw.Notify("first", "x")
	sw.Set(b) // reload swaps the chain
	sw.Notify("second", "y")

	require.Equal(t, []string{"first"}, a.got())
	require.Equal(t, []string{"second"}, b.got())
}

// TestSwitchNilDegradesToLog proves a nil backing (or nil replacement) degrades to
// a log-only notifier rather than panicking on delivery.
func TestSwitchNilDegradesToLog(t *testing.T) {
	sw := NewSwitch(nil)
	require.NotPanics(t, func() { sw.Notify("t", "b") })
	sw.Set(nil)
	require.NotPanics(t, func() { sw.Notify("t", "b") })
}
