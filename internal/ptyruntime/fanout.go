package ptyruntime

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/srjn45/warden/internal/ptyevent"
)

// Subscription errors.
var (
	// ErrResync: the subscriber fell behind (its buffer overflowed, or the
	// events it was replaying were trimmed). It must re-subscribe with its
	// cursor (the spec's "re-send hello"); the journal still has everything.
	ErrResync = errors.New("ptyruntime: subscriber fell behind; resubscribe from cursor")
	// ErrRejected: PlanResume rejected the cursor; discard state and attach fresh.
	ErrRejected = errors.New("ptyruntime: cursor rejected; attach fresh")
	// ErrClosed: the session was closed.
	ErrClosed = errors.New("ptyruntime: session closed")
)

// Subscription is one consumer of a session's redacted event stream: a
// journal replay from its cursor handed off, without loss, to the live feed
// (spec §6.3). C is closed when the subscription ends; Err says why.
type Subscription struct {
	// C delivers events in seq order (with the legal gap-marker holes).
	C <-chan ptyevent.Event
	// Plan is how the cursor was resolved. For ResumeSnapshot the first event
	// is the snapshot marker the client must reset its terminal model to.
	Plan ptyevent.ResumePlan
	// Head is the journal head when the subscription was made: events up to
	// it are replayed from the journal, later ones come from the live feed.
	Head uint64

	live   chan ptyevent.Event
	done   chan struct{}
	once   sync.Once
	lagged atomic.Bool
	mu     sync.Mutex
	err    error
	h      *hub
}

// Err reports why C closed (nil while open, or when the consumer closed it).
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Subscription) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// Close stops the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() { close(s.done) })
	s.h.dropSub(s)
}

// RawSubscription is a live, UNREDACTED byte feed for an interactive attach
// client — the user's own terminal (spec §5 redaction boundary). It never
// replays. When the client cannot keep up, C is closed and Lagged reports
// true; the client re-attaches. Close it when the client leaves.
type RawSubscription struct {
	C        <-chan []byte
	ClientID string

	ch     chan []byte
	once   sync.Once
	lagged atomic.Bool
	s      *Session
}

// Lagged reports whether the feed was cut because the client fell behind.
func (r *RawSubscription) Lagged() bool { return r.lagged.Load() }

// Close detaches the client (journaling lifecycle{detached}; a clean detach
// also takes a snapshot).
func (r *RawSubscription) Close() {
	r.once.Do(func() {
		r.s.hub.dropRaw(r)
		r.s.detach(r)
	})
}

// hub fans events out without ever blocking the publisher: a full subscriber
// buffer cuts that subscriber off (lag), it never stalls the PTY or writer.
type hub struct {
	mu     sync.Mutex
	subs   map[*Subscription]struct{}
	raws   map[*RawSubscription]struct{}
	closed bool
}

func newHub() *hub {
	return &hub{subs: map[*Subscription]struct{}{}, raws: map[*RawSubscription]struct{}{}}
}

func (h *hub) addSub(s *Subscription) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.subs[s] = struct{}{}
	return true
}

func (h *hub) dropSub(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.live)
	}
}

func (h *hub) addRaw(r *RawSubscription) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.raws[r] = struct{}{}
	return true
}

func (h *hub) dropRaw(r *RawSubscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.raws[r]; ok {
		delete(h.raws, r)
		close(r.ch)
	}
}

// publish hands a journaled event to every subscriber.
func (h *hub) publish(ev ptyevent.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.live <- ev:
		default:
			s.lagged.Store(true)
			delete(h.subs, s)
			close(s.live)
		}
	}
}

// raw hands a live PTY read to every interactive client. chunk is shared
// read-only.
func (h *hub) raw(chunk []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for r := range h.raws {
		select {
		case r.ch <- chunk:
		default:
			r.lagged.Store(true)
			delete(h.raws, r)
			close(r.ch)
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.live)
	}
	for r := range h.raws {
		delete(h.raws, r)
		close(r.ch)
	}
}
