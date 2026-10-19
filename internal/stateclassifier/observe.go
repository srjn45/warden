package stateclassifier

import (
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyobs"
)

// maxTrail bounds each session's in-memory transition trail (the journal is the
// durable record).
const maxTrail = 64

// adapterID names the session's backend for metric labels ("unknown" when it
// cannot be resolved; ptyobs collapses anything unbounded).
func (c *Classifier) adapterID(name string) string {
	if b := c.backendFor(name); b != nil {
		return b.ID()
	}
	return "unknown"
}

// countTransition records a committed transition and, for transitions out of
// activity, how long after the last terminal activity it was detected.
func (c *Classifier) countTransition(s *sessionState, obs ptyevent.Observation, ch *ptyevent.StateChange) {
	if c.Metrics == nil {
		return
	}
	ad := c.adapterID(s.name)
	c.Metrics.Inc(ptyobs.MTransitions, ad, string(ch.From), string(ch.To), string(ch.Reason))
	if ch.To != ptyevent.StateWorking && !s.lastActivity.IsZero() && obs.At.After(s.lastActivity) {
		c.Metrics.ObserveDuration(ptyobs.MDetectLatency, obs.At.Sub(s.lastActivity), ad, string(ch.To))
	}
}

// countInvestigation records what a quiescence investigation concluded.
func (c *Classifier) countInvestigation(name string, evs []ptyevent.Evidence, changed bool, now ptyevent.State) {
	if c.Metrics == nil {
		return
	}
	result := "no_change"
	has := func(k ptyevent.EvidenceKind) bool {
		for _, e := range evs {
			if e.Kind == k {
				return true
			}
		}
		return false
	}
	switch {
	case now == ptyevent.StateExited:
		result = "exited"
	case has(ptyevent.EvPrompt):
		result = "prompt"
	case has(ptyevent.EvUncertain):
		result = "unreadable"
	case changed:
		result = "transition"
	}
	c.Metrics.Inc(ptyobs.MInvestigations, c.adapterID(name), result)
}

// Status is the live classifier view of one session, for operator surfaces.
type Status struct {
	Snapshot ptyevent.Snapshot
	// Trail is the newest-last explainable transition trail (bounded).
	Trail []TransitionRecord
	// Prompt is the normalized prompt currently on screen, if any.
	Prompt *ptyevent.Prompt
	// Quiet is how long the terminal has been silent. Advisory only: it is
	// never a state (contract §14 t7).
	Quiet time.Duration
}

// Status returns the live view for name.
func (c *Classifier) Status(name string) (Status, bool) {
	c.mu.Lock()
	s, ok := c.sessions[name]
	c.mu.Unlock()
	if !ok {
		return Status{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Snapshot: s.snapshot, Trail: append([]TransitionRecord(nil), s.trail...)}
	if s.currentPrompt != nil {
		cp := *s.currentPrompt
		st.Prompt = &cp
	}
	if !s.lastActivity.IsZero() {
		st.Quiet = time.Since(s.lastActivity)
	}
	return st, true
}

// StateCounts returns the number of sessions per state (the sessions gauge).
func (c *Classifier) StateCounts() map[string]float64 {
	c.mu.Lock()
	all := make([]*sessionState, 0, len(c.sessions))
	for _, s := range c.sessions {
		all = append(all, s)
	}
	c.mu.Unlock()
	out := map[string]float64{}
	for _, s := range all {
		s.mu.Lock()
		out[string(s.snapshot.State)]++
		s.mu.Unlock()
	}
	return out
}

// Forget drops a session's state and stops its timer (agent deleted).
func (c *Classifier) Forget(name string) {
	c.mu.Lock()
	s, ok := c.sessions[name]
	delete(c.sessions, name)
	c.mu.Unlock()
	if !ok {
		return
	}
	s.mu.Lock()
	if s.quiescence != nil {
		s.quiescence.Stop()
	}
	s.mu.Unlock()
}
