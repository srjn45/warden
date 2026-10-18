package ptyevent

import "time"

// State is the normalized, provider-neutral session state.
type State string

const (
	// StateUnknown: no corroborated evidence yet (fresh session, recovered
	// runtime before the first classification).
	StateUnknown State = "unknown"
	// StateWorking: the CLI is executing a turn.
	StateWorking State = "working"
	// StateIdle: the CLI is at rest at its input box, awaiting a new turn.
	StateIdle State = "idle"
	// StateWaiting: blocked on something external; Reason says what.
	StateWaiting State = "waiting"
	// StateWaitingForPermission: a confidently classified permission prompt is
	// on screen.
	StateWaitingForPermission State = "waiting_for_permission"
	// StateWaitingForDecision: a confidently classified decision
	// (strategic / multiple-choice / free-text) prompt is on screen.
	StateWaitingForDecision State = "waiting_for_decision"
	// StateNeedsAttention: something is on screen warden cannot classify
	// confidently. Never acted on automatically.
	StateNeedsAttention State = "needs_attention"
	// StateExited: the process ended (terminal for the epoch).
	StateExited State = "exited"
)

// Valid reports whether s is known.
func (s State) Valid() bool {
	switch s {
	case StateUnknown, StateWorking, StateIdle, StateWaiting, StateWaitingForPermission,
		StateWaitingForDecision, StateNeedsAttention, StateExited:
		return true
	}
	return false
}

// WaitingReason types StateWaiting.
type WaitingReason string

const (
	ReasonNone       WaitingReason = ""
	ReasonStartup    WaitingReason = "startup"    // spawned, readiness marker not seen yet
	ReasonRateLimit  WaitingReason = "rate_limit" // short-window limit; resets at a known/unknown time
	ReasonQuota      WaitingReason = "quota"      // spend/plan quota exhausted
	ReasonAuth       WaitingReason = "auth"       // login/re-auth required
	ReasonNetwork    WaitingReason = "network"    // CLI retrying a connection
	ReasonSubprocess WaitingReason = "subprocess" // a long-running tool child with no CLI output
)

// Valid reports whether r is known.
func (r WaitingReason) Valid() bool {
	switch r {
	case ReasonNone, ReasonStartup, ReasonRateLimit, ReasonQuota, ReasonAuth, ReasonNetwork, ReasonSubprocess:
		return true
	}
	return false
}

// EvidenceKind is one observation type. See the spec's §6 evidence table for
// which kinds are strong (commit immediately), positive (subject to
// debounce/hysteresis) or corroborating-only (never sufficient alone).
type EvidenceKind string

const (
	EvProcessSpawn   EvidenceKind = "process_spawn"   // strong
	EvProcessExit    EvidenceKind = "process_exit"    // strong
	EvReady          EvidenceKind = "ready"           // strong: backend readiness marker (seed gate)
	EvHook           EvidenceKind = "hook"            // strong: backend-native lifecycle hook (Hook field)
	EvWorkingMarker  EvidenceKind = "working_marker"  // positive: adapter's busy marker on screen
	EvIdleMarker     EvidenceKind = "idle_marker"     // positive: adapter's input-ready marker on screen
	EvPrompt         EvidenceKind = "prompt"          // positive: normalized prompt on screen (Prompt field)
	EvPromptCleared  EvidenceKind = "prompt_cleared"  // corroborating
	EvLimitBanner    EvidenceKind = "limit_banner"    // positive: limit/auth/network banner (Reason field)
	EvUncertain      EvidenceKind = "uncertain"       // positive: classifier saw a blocking screen it cannot read
	EvOutputActivity EvidenceKind = "output_activity" // corroborating: non-trivial output bytes
	EvInput          EvidenceKind = "input"           // corroborating: input written (Origin field)
	EvTurnStart      EvidenceKind = "turn_start"      // corroborating: transcript reader saw a new turn
	EvTurnEnd        EvidenceKind = "turn_end"        // corroborating: transcript reader saw a turn finish
	EvSilence        EvidenceKind = "silence"         // corroborating: no output for Quiet (NEVER sufficient alone)
)

// Evidence is one observation.
type Evidence struct {
	Kind EvidenceKind `json:"kind"`
	// Reason for EvLimitBanner (and for EvHook targeting StateWaiting).
	Reason WaitingReason `json:"reason,omitempty"`
	// Prompt for EvPrompt; LearnedUsable is the store verdict for a learned one.
	Prompt        *Prompt `json:"prompt,omitempty"`
	LearnedUsable bool    `json:"learned_usable,omitempty"`
	// Hook is the state a backend hook asserts (EvHook).
	Hook State `json:"hook,omitempty"`
	// Origin for EvInput.
	Origin Origin `json:"origin,omitempty"`
	// Quiet for EvSilence.
	Quiet time.Duration `json:"quiet,omitempty"`
}

// Observation is the evidence gathered at one instant (one classifier pass
// over the journal head). The runtime produces observations on EVENTS
// (output, input, lifecycle, hook, transcript append) and on a coarse
// silence timer — never by polling the screen.
type Observation struct {
	At       time.Time  `json:"at"`
	Seq      uint64     `json:"seq"`
	Evidence []Evidence `json:"evidence"`
}

func (o Observation) has(k EvidenceKind) bool { return o.find(k) != nil }

func (o Observation) find(k EvidenceKind) *Evidence {
	for i := range o.Evidence {
		if o.Evidence[i].Kind == k {
			return &o.Evidence[i]
		}
	}
	return nil
}

// Hysteresis is the debounce per target state: a non-strong proposal commits
// once it has been proposed MinHits times (in distinct observations) spanning
// at least Dwell, with no contradicting proposal in between. A pending
// candidate not re-proposed within PendingTTL is discarded.
type Hysteresis struct {
	Dwell      map[State]time.Duration
	MinHits    map[State]int
	PendingTTL time.Duration
	// IdleQuiet is how much silence corroborates an idle marker on its own
	// first sighting (idle marker + silence ≥ IdleQuiet commits at once).
	IdleQuiet time.Duration
}

// DefaultHysteresis is the contract default.
var DefaultHysteresis = Hysteresis{
	Dwell: map[State]time.Duration{
		StateWorking:              0,
		StateIdle:                 1500 * time.Millisecond,
		StateWaiting:              time.Second,
		StateWaitingForPermission: 200 * time.Millisecond,
		StateWaitingForDecision:   200 * time.Millisecond,
		StateNeedsAttention:       2 * time.Second,
	},
	MinHits: map[State]int{
		StateWorking:              1,
		StateIdle:                 2,
		StateWaiting:              2,
		StateWaitingForPermission: 2,
		StateWaitingForDecision:   2,
		StateNeedsAttention:       2,
	},
	PendingTTL: 30 * time.Second,
	IdleQuiet:  5 * time.Second,
}

// Candidate is a proposed, not-yet-committed transition.
type Candidate struct {
	State     State         `json:"state"`
	Reason    WaitingReason `json:"reason,omitempty"`
	FirstSeen time.Time     `json:"first_seen"`
	LastSeen  time.Time     `json:"last_seen"`
	Hits      int           `json:"hits"`
}

// Snapshot is the reducer state for one session.
type Snapshot struct {
	State   State         `json:"state"`
	Reason  WaitingReason `json:"reason,omitempty"`
	Since   time.Time     `json:"since"`
	Pending *Candidate    `json:"pending,omitempty"`
}

// NewSnapshot is the initial reducer state.
func NewSnapshot(at time.Time) Snapshot { return Snapshot{State: StateUnknown, Since: at} }

type proposal struct {
	state    State
	reason   WaitingReason
	strong   bool
	evidence []EvidenceKind
}

// propose derives at most one target state from an observation, in priority
// order. It encodes the "never from silence alone" rule: silence, output
// activity, input, prompt-cleared and transcript turn boundaries only ever
// corroborate a screen/hook/process signal.
func propose(cur Snapshot, o Observation, h Hysteresis) (proposal, bool) {
	if o.has(EvProcessExit) {
		return proposal{StateExited, ReasonNone, true, []EvidenceKind{EvProcessExit}}, true
	}
	if o.has(EvProcessSpawn) {
		return proposal{StateWaiting, ReasonStartup, true, []EvidenceKind{EvProcessSpawn}}, true
	}
	if cur.State == StateExited {
		return proposal{}, false // only a new spawn leaves exited
	}
	if e := o.find(EvHook); e != nil && e.Hook.Valid() && e.Hook != StateUnknown && e.Hook != StateExited {
		r := ReasonNone
		if e.Hook == StateWaiting {
			r = e.Reason
		}
		return proposal{e.Hook, r, true, []EvidenceKind{EvHook}}, true
	}
	if o.has(EvReady) && (cur.State == StateUnknown || (cur.State == StateWaiting && cur.Reason == ReasonStartup)) {
		return proposal{StateIdle, ReasonNone, true, []EvidenceKind{EvReady}}, true
	}
	if o.has(EvUncertain) {
		return proposal{StateNeedsAttention, ReasonNone, false, []EvidenceKind{EvUncertain}}, true
	}
	if e := o.find(EvPrompt); e != nil {
		switch {
		case e.Prompt == nil || !Confident(e.Prompt, e.LearnedUsable):
			return proposal{StateNeedsAttention, ReasonNone, false, []EvidenceKind{EvPrompt}}, true
		case e.Prompt.Class == ClassPermission:
			return proposal{StateWaitingForPermission, ReasonNone, false, []EvidenceKind{EvPrompt}}, true
		case e.Prompt.Class == ClassDecision:
			return proposal{StateWaitingForDecision, ReasonNone, false, []EvidenceKind{EvPrompt}}, true
		}
	}
	if e := o.find(EvLimitBanner); e != nil && e.Reason != ReasonNone && e.Reason.Valid() {
		return proposal{StateWaiting, e.Reason, false, []EvidenceKind{EvLimitBanner}}, true
	}
	if o.has(EvWorkingMarker) {
		return proposal{StateWorking, ReasonNone, false, []EvidenceKind{EvWorkingMarker}}, true
	}
	if o.has(EvOutputActivity) && (o.has(EvInput) || o.has(EvPromptCleared) || o.has(EvTurnStart)) {
		ev := []EvidenceKind{EvOutputActivity}
		for _, k := range []EvidenceKind{EvInput, EvPromptCleared, EvTurnStart} {
			if o.has(k) {
				ev = append(ev, k)
			}
		}
		return proposal{StateWorking, ReasonNone, false, ev}, true
	}
	if o.has(EvIdleMarker) {
		ev := []EvidenceKind{EvIdleMarker}
		corroborated := false
		if s := o.find(EvSilence); s != nil && s.Quiet >= h.IdleQuiet {
			ev, corroborated = append(ev, EvSilence), true
		}
		if o.has(EvTurnEnd) {
			ev, corroborated = append(ev, EvTurnEnd), true
		}
		// Idle marker + corroboration commits on first sighting; a bare idle
		// marker must be re-observed through the dwell (hysteresis).
		return proposal{StateIdle, ReasonNone, corroborated, ev}, true
	}
	return proposal{}, false
}

// Step folds one observation into the snapshot and returns the committed
// transition, if any. It is pure: identical inputs give identical outputs.
func Step(cur Snapshot, o Observation, h Hysteresis) (Snapshot, *StateChange) {
	if cur.Pending != nil && h.PendingTTL > 0 && o.At.Sub(cur.Pending.LastSeen) > h.PendingTTL {
		cur.Pending = nil
	}
	p, ok := propose(cur, o, h)
	if !ok {
		return cur, nil // no signal (e.g. silence alone): hold state, keep pending
	}
	if p.state == cur.State && p.reason == cur.Reason {
		cur.Pending = nil // current state reaffirmed; any contrary candidate is void
		return cur, nil
	}
	if !p.strong {
		c := cur.Pending
		if c == nil || c.State != p.state || c.Reason != p.reason {
			c = &Candidate{State: p.state, Reason: p.reason, FirstSeen: o.At}
		} else {
			c = &Candidate{State: c.State, Reason: c.Reason, FirstSeen: c.FirstSeen, Hits: c.Hits}
		}
		c.Hits++
		c.LastSeen = o.At
		minHits, dwell := h.MinHits[p.state], h.Dwell[p.state]
		if minHits < 1 {
			minHits = 1
		}
		if c.Hits < minHits || o.At.Sub(c.FirstSeen) < dwell {
			cur.Pending = c
			return cur, nil
		}
	}
	ch := &StateChange{From: cur.State, To: p.state, Reason: p.reason, Evidence: p.evidence}
	return Snapshot{State: p.state, Reason: p.reason, Since: o.At}, ch
}
