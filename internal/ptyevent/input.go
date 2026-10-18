package ptyevent

import "time"

// Origin is who wrote input (or requested a resize).
type Origin string

const (
	// OriginHuman is a person typing in an attached client (TUI, web, app,
	// `tmux attach`). Human input is never blocked or delayed by warden.
	OriginHuman Origin = "human"
	// OriginOperator is an explicit API/CLI/MCP send on someone's behalf
	// (send_to_agent, `wd send`, an orchestrator's message). It is deliberate
	// but not interactive: it yields to a human mid-line.
	OriginOperator Origin = "operator"
	// OriginWarden is automatic input: a decision-router answer, the initial
	// prompt seed, a rate-limit resume. It must carry a DecisionID (or a seed /
	// resume reason) and it yields to both humans and in-flight operator input.
	OriginWarden Origin = "warden"
)

// Valid reports whether o is a known origin.
func (o Origin) Valid() bool { return o == OriginHuman || o == OriginOperator || o == OriginWarden }

// OwnershipConfig tunes input arbitration. Defaults (DefaultOwnership) are
// generous: they only bite when a human is actually typing.
type OwnershipConfig struct {
	// HumanQuiet is how long after the last human keystroke the human keeps
	// the input lease. Warden/operator input arriving inside it is deferred.
	HumanQuiet time.Duration
	// MaxDefer bounds how long a deferred automatic write may wait before the
	// router must give up and re-decide from fresh evidence (TOCTOU).
	MaxDefer time.Duration
}

// DefaultOwnership is the contract default.
var DefaultOwnership = OwnershipConfig{HumanQuiet: 3 * time.Second, MaxDefer: 30 * time.Second}

// Lease is the per-session input-ownership state the runtime keeps.
type Lease struct {
	// Holder is the origin currently owning the line ("" = free).
	Holder Origin
	// ClientID of the human client holding it (when Holder is human).
	ClientID string
	// LastHumanAt is the time of the last human keystroke (zero = never).
	LastHumanAt time.Time
	// HumanLineOpen is true when the human typed since their last Enter: a
	// half-typed line that an automatic write would corrupt. It keeps the
	// lease beyond HumanQuiet until the human submits or clears the line.
	HumanLineOpen bool
	// HumanSeq is the journal seq of the last human input (for Revalidate).
	HumanSeq uint64
}

// InputRequest asks to write Data now.
type InputRequest struct {
	Origin   Origin
	ClientID string
	At       time.Time
	Seq      uint64 // seq the write would be journaled at (human writes update HumanSeq)
	Data     []byte
	// DecisionID is required for OriginWarden.
	DecisionID string
	// Reason labels a warden write that is not a prompt decision (seed, resume).
	Reason string
	// FirstDeferredAt is when this request was first deferred (zero = first try).
	FirstDeferredAt time.Time
}

// Verdict is the arbitration outcome.
type Verdict string

const (
	// VerdictWrite: write the bytes now.
	VerdictWrite Verdict = "write"
	// VerdictDefer: retry after RetryAfter (a human holds the line).
	VerdictDefer Verdict = "defer"
	// VerdictAbandon: the automatic write waited past MaxDefer; drop it and let
	// the router re-decide from fresh evidence.
	VerdictAbandon Verdict = "abandon"
	// VerdictReject: the request is malformed (unknown origin, warden write
	// without a DecisionID/Reason).
	VerdictReject Verdict = "reject"
)

// Arbitration is Arbitrate's result.
type Arbitration struct {
	Verdict    Verdict
	RetryAfter time.Duration
	// PreemptsWarden is true when a human write lands while warden had queued
	// or in-flight automatic input: the router must cancel pending decisions
	// (their Revalidate will fail on HumanSeq anyway).
	PreemptsWarden bool
	Reason         string
}

// Arbitrate decides whether req may write now and returns the updated lease.
// The rules, in order:
//
//  1. Human input is always written immediately and takes the lease.
//  2. Malformed automatic input is rejected.
//  3. Automatic input (operator, warden) is deferred while a human holds the
//     lease: within HumanQuiet of their last keystroke, or with a half-typed
//     line open. A warden write deferred past MaxDefer is abandoned (an
//     operator write is not — the operator asked for it explicitly; it simply
//     waits for the human).
//  4. Otherwise the write proceeds.
func Arbitrate(l Lease, req InputRequest, cfg OwnershipConfig) (Lease, Arbitration) {
	switch req.Origin {
	case OriginHuman:
		pre := l.Holder == OriginWarden || l.Holder == OriginOperator
		l.Holder, l.ClientID, l.LastHumanAt, l.HumanSeq = OriginHuman, req.ClientID, req.At, req.Seq
		l.HumanLineOpen = humanLineOpen(l.HumanLineOpen, req.Data)
		return l, Arbitration{Verdict: VerdictWrite, PreemptsWarden: pre, Reason: "human input always wins"}
	case OriginOperator, OriginWarden:
	default:
		return l, Arbitration{Verdict: VerdictReject, Reason: "unknown origin"}
	}
	if req.Origin == OriginWarden && req.DecisionID == "" && req.Reason == "" {
		return l, Arbitration{Verdict: VerdictReject, Reason: "warden input must cite a decision or a reason"}
	}
	if held, wait := humanHolds(l, req.At, cfg); held {
		if req.Origin == OriginWarden && !req.FirstDeferredAt.IsZero() && req.At.Sub(req.FirstDeferredAt) >= cfg.MaxDefer {
			return l, Arbitration{Verdict: VerdictAbandon, Reason: "human kept the line past max defer; re-decide"}
		}
		return l, Arbitration{Verdict: VerdictDefer, RetryAfter: wait, Reason: "human holds the input lease"}
	}
	l.Holder, l.ClientID = req.Origin, req.ClientID
	return l, Arbitration{Verdict: VerdictWrite, Reason: "line free"}
}

// Release marks an automatic write complete so the line is free again.
func Release(l Lease, o Origin) Lease {
	if l.Holder == o && o != OriginHuman {
		l.Holder, l.ClientID = "", ""
	}
	return l
}

func humanHolds(l Lease, now time.Time, cfg OwnershipConfig) (bool, time.Duration) {
	if l.LastHumanAt.IsZero() {
		return false, 0
	}
	since := now.Sub(l.LastHumanAt)
	if since < cfg.HumanQuiet {
		return true, cfg.HumanQuiet - since
	}
	if l.HumanLineOpen {
		return true, cfg.HumanQuiet
	}
	return false, 0
}

// humanLineOpen tracks whether the human has an unsubmitted line: Enter (CR or
// LF) closes it, Ctrl-C / Ctrl-U / Escape clear it, anything else opens it.
func humanLineOpen(open bool, data []byte) bool {
	for _, b := range data {
		switch b {
		case '\r', '\n', 0x03, 0x15:
			open = false
		case 0x1b:
			// A lone Escape clears; an escape sequence (arrow keys) does not type.
			if len(data) == 1 {
				open = false
			}
			return open
		default:
			open = true
		}
	}
	return open
}
