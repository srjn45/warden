package tui

import (
	"context"
	"math/rand"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/store"
)

const (
	baseRefreshInterval         = 1 * time.Second
	conservativeRefreshInterval = 5 * time.Second
	maxRefreshBackoff           = 15 * time.Second
	refreshBackoffFactor        = 1.5
	refreshJitterRatio          = 0.25
)

// sseSnapshotMsg delivers an SSE session snapshot from the daemon to the TUI.
type sseSnapshotMsg struct {
	sessions []*store.Session
	err      error
}

// computeRefreshBackoff calculates bounded exponential backoff with randomized jitter.
func computeRefreshBackoff(base time.Duration, failures int, maxBackoff time.Duration, jitterRand func() float64) time.Duration {
	if failures <= 0 {
		return base
	}
	cur := float64(base)
	for i := 0; i < failures && cur < float64(maxBackoff); i++ {
		cur *= refreshBackoffFactor
	}
	if cur > float64(maxBackoff) {
		cur = float64(maxBackoff)
	}
	jitter := 0.0
	if jitterRand != nil {
		jitter = cur * refreshJitterRatio * jitterRand()
	} else {
		jitter = cur * refreshJitterRatio * rand.Float64()
	}
	d := time.Duration(cur + jitter)
	if d > maxBackoff+time.Second {
		d = maxBackoff + time.Second
	}
	return d
}

func tickWithDuration(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// sseWatcher is optionally implemented by the api client.
type sseWatcher interface {
	WatchAll(ctx context.Context, onSnapshot func([]*store.Session) error) error
}

// subscribeSSECmd starts a background goroutine streaming SSE snapshots into ch.
func subscribeSSECmd(a api, ch chan sseSnapshotMsg) tea.Cmd {
	w, ok := a.(sseWatcher)
	if !ok || ch == nil {
		return nil
	}
	go func() {
		for {
			err := w.WatchAll(context.Background(), func(ss []*store.Session) error {
				ch <- sseSnapshotMsg{sessions: ss}
				return nil
			})
			if err != nil {
				ch <- sseSnapshotMsg{err: err}
			}
			time.Sleep(2 * time.Second)
		}
	}()
	return waitForSSEMsg(ch)
}

func waitForSSEMsg(ch chan sseSnapshotMsg) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		return <-ch
	}
}
