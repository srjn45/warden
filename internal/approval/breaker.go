package approval

import (
	"sync"
	"time"
)

// DefaultMaxRepeats is the consecutive-identical-approval cap applied when a
// policy leaves MaxRepeats unset (0). Deliberately generous: a legitimate
// re-prompt rarely repeats byte-for-byte more than a couple of times, whereas a
// blocked agent re-running a failing command re-asks the identical question
// every few seconds, forever.
const DefaultMaxRepeats = 10

// breakerCooldown is how long an agent's run must stay quiet (no identical
// prompt observed, approved or denied) before a tripped breaker re-arms. A
// live loop keeps refreshing the run and stays blocked; once a human answers
// or the agent moves on, a later genuine re-ask starts a fresh run.
const breakerCooldown = 10 * time.Minute

// Breaker is a per-agent circuit breaker for auto-approval. It counts how many
// times the SAME prompt (by signature) has been consecutively auto-approved
// for an agent; once the count reaches the cap, approving clearly is not
// unblocking the agent (it re-runs a failing command and re-asks), so further
// identical approvals are denied and the caller escalates to a human. A
// different prompt resets the run.
type Breaker struct {
	mu    sync.Mutex
	state map[string]breakerRun
	now   func() time.Time // injectable clock for tests
}

type breakerRun struct {
	sig     string
	count   int
	tripped bool
	last    time.Time
}

// NewBreaker returns an empty breaker.
func NewBreaker() *Breaker {
	return &Breaker{state: map[string]breakerRun{}, now: time.Now}
}

// Allow reports whether one more auto-approval of the prompt with signature
// sig may be sent for agent id, capped at max consecutive identical approvals
// (max <= 0 disables the breaker entirely). trippedNow is true exactly once
// per trip — on the first denial — so the caller can raise a one-shot alert.
// A different sig, or a cooldown's worth of quiet, resets the run.
func (b *Breaker) Allow(id, sig string, max int) (ok, trippedNow bool) {
	if max <= 0 {
		return true, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	r := b.state[id]
	if r.sig != sig || now.Sub(r.last) > breakerCooldown {
		r = breakerRun{sig: sig}
	}
	r.last = now
	if r.count >= max {
		trippedNow = !r.tripped
		r.tripped = true
		b.state[id] = r
		return false, trippedNow
	}
	r.count++
	b.state[id] = r
	return true, false
}

// Reset forgets the agent's run (e.g. the agent was terminated or restored).
func (b *Breaker) Reset(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, id)
}

// Len reports how many agents currently hold a run.
func (b *Breaker) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.state)
}

// Prune drops runs for agents no longer in live, so the state map cannot grow
// without bound over a long-running daemon.
func (b *Breaker) Prune(live map[string]struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.state {
		if _, ok := live[id]; !ok {
			delete(b.state, id)
		}
	}
}
