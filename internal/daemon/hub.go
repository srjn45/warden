package daemon

import "sync"

// hub is a tiny in-process broadcaster. Subscribers get a coalescing (cap-1)
// channel: publish() never blocks, and bursts collapse into one pending tick.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan struct{}]struct{})}
}

func (h *hub) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			close(ch)
			h.mu.Unlock()
		})
	}
	return ch, unsub
}

func (h *hub) publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // already has a pending tick — coalesce
		}
	}
}
