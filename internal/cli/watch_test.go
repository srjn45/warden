package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func idsOf(ss []*store.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// TestWatchLoopNeverConnectedReturnsError: a watch that never establishes (daemon
// down) is surfaced, not retried forever — `warden ls --watch` fails fast.
func TestWatchLoopNeverConnectedReturnsError(t *testing.T) {
	watch := func(context.Context, func([]*store.Session) error) error { return client.ErrDaemonDown }
	err := watchLoop(context.Background(), watch,
		func([]*store.Session) error { return nil }, nil,
		func(context.Context, time.Duration) bool { return true })
	require.ErrorIs(t, err, client.ErrDaemonDown)
}

// TestWatchLoopGracefulEndExits: a stream the server closes cleanly (nil return,
// e.g. a one-shot snapshot) is a clean exit — it must NOT be treated as a drop and
// reconnected, or `warden ls --watch` would spin forever on a graceful close.
func TestWatchLoopGracefulEndExits(t *testing.T) {
	var rendered [][]*store.Session
	calls := 0
	watch := func(_ context.Context, on func([]*store.Session) error) error {
		calls++
		_ = on([]*store.Session{{ID: "W-1"}})
		return nil // server closed the stream gracefully
	}
	render := func(ss []*store.Session) error { rendered = append(rendered, ss); return nil }

	err := watchLoop(context.Background(), watch, render,
		func(error, time.Duration) { t.Fatal("graceful end must not fire onDrop") },
		func(context.Context, time.Duration) bool { t.Fatal("graceful end must not back off"); return false })

	require.NoError(t, err)
	require.Equal(t, 1, calls, "a graceful stream end exits without reconnecting")
	require.Len(t, rendered, 1)
	require.Equal(t, []string{"W-1"}, idsOf(rendered[0]))
}

// TestWatchLoopReconnectsAfterDrop: an established stream that drops reconnects and
// keeps rendering — the fleet is not torn down by a transient disconnect.
func TestWatchLoopReconnectsAfterDrop(t *testing.T) {
	var rendered [][]*store.Session
	calls, drops := 0, 0
	watch := func(_ context.Context, on func([]*store.Session) error) error {
		calls++
		if calls == 1 {
			_ = on([]*store.Session{{ID: "a1"}}) // established, then dropped
			return errors.New("stream dropped")
		}
		_ = on([]*store.Session{{ID: "a1"}, {ID: "a2"}}) // reconnected with a fresh snapshot
		return context.Canceled                          // user Ctrl+C: clean stop
	}
	render := func(ss []*store.Session) error { rendered = append(rendered, ss); return nil }
	onDrop := func(error, time.Duration) { drops++ }

	err := watchLoop(context.Background(), watch, render, onDrop,
		func(context.Context, time.Duration) bool { return true })

	require.NoError(t, err)
	require.Equal(t, 2, calls, "reconnected after the drop")
	require.Equal(t, 1, drops, "one non-blocking drop notice")
	require.Len(t, rendered, 2)
	require.Equal(t, []string{"a1"}, idsOf(rendered[0]))
	require.Equal(t, []string{"a1", "a2"}, idsOf(rendered[1]), "reconnect renders the newest complete snapshot")
}

// TestWatchLoopBackoffGrows: repeated drops without a delivered snapshot grow the
// reconnect backoff exponentially, capped — a delivered snapshot resets it.
func TestWatchLoopBackoffGrows(t *testing.T) {
	calls := 0
	watch := func(_ context.Context, on func([]*store.Session) error) error {
		calls++
		if calls == 1 {
			_ = on([]*store.Session{{ID: "a1"}}) // establish + deliver, then drop
		}
		return errors.New("drop")
	}
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return len(waits) < 6 // cancel after the 6th backoff wait
	}

	err := watchLoop(context.Background(), watch,
		func([]*store.Session) error { return nil }, nil, sleep)

	require.NoError(t, err)
	require.Equal(t, watchBackoffBase, waits[0], "first wait after a delivering stream is the base")
	require.Equal(t, 2*watchBackoffBase, waits[1])
	require.Equal(t, 4*watchBackoffBase, waits[2])
	require.Equal(t, watchBackoffCap, waits[len(waits)-1], "backoff is capped")
}

// TestWatchLoopRenderErrorFatal: an output/render failure (broken pipe) ends the
// watch — it is not a reconnect case.
func TestWatchLoopRenderErrorFatal(t *testing.T) {
	boom := errors.New("broken pipe")
	watch := func(_ context.Context, on func([]*store.Session) error) error {
		return on([]*store.Session{{ID: "a1"}})
	}
	err := watchLoop(context.Background(), watch,
		func([]*store.Session) error { return boom }, nil,
		func(context.Context, time.Duration) bool { return true })
	require.ErrorIs(t, err, boom)
}

// TestWatchLoopCancelDuringBackoff: cancelling while waiting to reconnect stops
// cleanly without another connect attempt.
func TestWatchLoopCancelDuringBackoff(t *testing.T) {
	calls := 0
	watch := func(_ context.Context, on func([]*store.Session) error) error {
		calls++
		_ = on([]*store.Session{{ID: "a1"}})
		return errors.New("drop")
	}
	err := watchLoop(context.Background(), watch,
		func([]*store.Session) error { return nil }, nil,
		func(context.Context, time.Duration) bool { return false }) // cancelled during wait
	require.NoError(t, err)
	require.Equal(t, 1, calls, "cancel during backoff stops before reconnecting")
}
