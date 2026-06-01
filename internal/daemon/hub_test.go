package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHubPublishWakesSubscriber(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	h.publish()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber was not notified")
	}
}

func TestHubCoalesces(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	h.publish()
	h.publish()
	h.publish()
	// cap-1 channel collapses bursts into a single pending notification.
	<-ch
	select {
	case <-ch:
		t.Fatal("expected coalesced single notification")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	unsub()
	h.publish()
	select {
	case _, ok := <-ch:
		require.False(t, ok, "channel should be closed after unsubscribe")
	case <-time.After(50 * time.Millisecond):
		t.Fatal("expected closed channel")
	}
}

func TestHubConcurrentPublishIsRaceClean(t *testing.T) {
	h := newHub()
	ch, unsub := h.subscribe()
	defer unsub()
	go func() {
		for i := 0; i < 100; i++ {
			<-ch
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.publish() }()
	}
	wg.Wait()
}
