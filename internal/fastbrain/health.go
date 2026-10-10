package fastbrain

import (
	"sync"
	"time"
)

// FailureClass classifies a runner failure for health accounting. Caller
// cancellation is deliberately not a class: it says nothing about the runner.
type FailureClass string

const (
	FailTimeout     FailureClass = "timeout"
	FailRunnerError FailureClass = "runner_error"
)

// CircuitState is the health-circuit state of one runner candidate.
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

// HealthOptions tunes the per-runner circuit breaker. Defaults follow the
// decision-inventory policy: open after 3 consecutive failures or >= 50 % over
// the last 10 calls; half-open probes after 30 s, then 2 min, then 10 min.
type HealthOptions struct {
	ConsecutiveFailures int
	Window              int
	FailRatio           float64
	Cooldowns           []time.Duration
	Now                 func() time.Time
}

func (o HealthOptions) normalized() HealthOptions {
	if o.ConsecutiveFailures <= 0 {
		o.ConsecutiveFailures = 3
	}
	if o.Window <= 0 {
		o.Window = 10
	}
	if o.FailRatio <= 0 {
		o.FailRatio = 0.5
	}
	if len(o.Cooldowns) == 0 {
		o.Cooldowns = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// RunnerHealth is a content-free snapshot of one candidate's health, safe for
// metrics and operator surfaces (ID is a bounded backend label).
type RunnerHealth struct {
	ID                  string
	State               CircuitState
	ConsecutiveFailures int
	LastFailure         FailureClass
	RetryAt             time.Time // zero unless open
	Opens               int       // times the circuit has opened
}

type healthEntry struct {
	state       CircuitState
	consecutive int
	window      []bool // true = failure; ring of the last Window outcomes
	last        FailureClass
	openedAt    time.Time
	level       int // index into Cooldowns for the next/current cooldown
	probing     bool
	opens       int
}

// Health tracks per-runner circuit state. It is safe for concurrent use and
// holds no prompt or output content.
type Health struct {
	mu      sync.Mutex
	opts    HealthOptions
	entries map[string]*healthEntry
}

// NewHealth returns an empty tracker.
func NewHealth(opts HealthOptions) *Health {
	return &Health{opts: opts.normalized(), entries: make(map[string]*healthEntry)}
}

func (h *Health) entry(id string) *healthEntry {
	e := h.entries[id]
	if e == nil {
		e = &healthEntry{state: CircuitClosed}
		h.entries[id] = e
	}
	return e
}

func (h *Health) cooldown(level int) time.Duration {
	if level >= len(h.opts.Cooldowns) {
		level = len(h.opts.Cooldowns) - 1
	}
	return h.opts.Cooldowns[level]
}

// Peek reports whether Allow would currently admit a call, without claiming a
// probe slot. Selection uses it to plan fallbacks.
func (h *Health) Peek(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entries[id]
	if e == nil || e.state == CircuitClosed {
		return true
	}
	if e.state == CircuitHalfOpen {
		return !e.probing
	}
	return !h.opts.Now().Before(e.openedAt.Add(h.cooldown(e.level)))
}

// Allow admits a call to id. A closed circuit always admits. An open circuit
// admits exactly one bounded probe once its cooldown elapsed (moving to
// half-open); further calls are refused until the probe reports a verdict.
func (h *Health) Allow(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(id)
	switch e.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if h.opts.Now().Before(e.openedAt.Add(h.cooldown(e.level))) {
			return false
		}
		e.state, e.probing = CircuitHalfOpen, true
		return true
	default: // half-open
		if e.probing {
			return false
		}
		e.probing = true
		return true
	}
}

// Success records a good call and closes the circuit.
func (h *Health) Success(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(id)
	e.state, e.consecutive, e.probing, e.level = CircuitClosed, 0, false, 0
	e.last = ""
	h.push(e, false)
}

// Failure records a classified failure and opens the circuit when the policy
// trips. A failed half-open probe reopens with the next, longer cooldown.
func (h *Health) Failure(id string, class FailureClass) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(id)
	e.last = class
	e.consecutive++
	h.push(e, true)
	if e.state == CircuitHalfOpen {
		e.probing = false
		e.level++
		h.open(e)
		return
	}
	if e.state == CircuitOpen {
		return
	}
	fails := 0
	for _, f := range e.window {
		if f {
			fails++
		}
	}
	ratioTrip := len(e.window) >= h.opts.Window && float64(fails)/float64(len(e.window)) >= h.opts.FailRatio
	if e.consecutive >= h.opts.ConsecutiveFailures || ratioTrip {
		h.open(e)
	}
}

// Release gives back a probe slot when a call ended without a verdict (for
// example the caller canceled), so the circuit is not wedged half-open.
func (h *Health) Release(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entries[id]; e != nil {
		e.probing = false
	}
}

func (h *Health) open(e *healthEntry) {
	e.state = CircuitOpen
	e.openedAt = h.opts.Now()
	e.opens++
}

func (h *Health) push(e *healthEntry, failed bool) {
	e.window = append(e.window, failed)
	if len(e.window) > h.opts.Window {
		e.window = e.window[len(e.window)-h.opts.Window:]
	}
}

// State returns the current circuit state for id (closed if unknown).
func (h *Health) State(id string) CircuitState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.entries[id]; e != nil {
		return e.state
	}
	return CircuitClosed
}

// Snapshot returns a copy of every tracked candidate's health.
func (h *Health) Snapshot() []RunnerHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RunnerHealth, 0, len(h.entries))
	for id, e := range h.entries {
		rh := RunnerHealth{ID: id, State: e.state, ConsecutiveFailures: e.consecutive, LastFailure: e.last, Opens: e.opens}
		if e.state == CircuitOpen {
			rh.RetryAt = e.openedAt.Add(h.cooldown(e.level))
		}
		out = append(out, rh)
	}
	return out
}
