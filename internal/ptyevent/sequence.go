package ptyevent

import "fmt"

// Order is the result of checking an event against the previous one a consumer
// applied.
type Order string

const (
	OrderNext       Order = "next"       // seq == prev+1: apply it
	OrderDuplicate  Order = "duplicate"  // seq <= prev: already applied (replay overlap); drop it
	OrderGap        Order = "gap"        // seq > prev+1: events were missed; resynchronize
	OrderForeign    Order = "foreign"    // a different session's event: protocol error
	OrderRegression Order = "regression" // epoch went backwards: protocol error
)

// CheckNext classifies ev against the last event a consumer applied (prev; the
// zero Event means "nothing applied yet", so the first acceptable seq is 1).
// Consumers MUST apply only OrderNext, silently drop OrderDuplicate (reconnect
// overlap is expected), and on OrderGap stop applying and resume via PlanResume
// — never paper over a gap by applying later bytes to a stale screen.
func CheckNext(prev Event, ev Event) Order {
	if prev.SessionID != "" && ev.SessionID != prev.SessionID {
		return OrderForeign
	}
	if prev.Seq != 0 && ev.Epoch < prev.Epoch {
		return OrderRegression
	}
	switch {
	case ev.Seq <= prev.Seq:
		return OrderDuplicate
	case ev.Seq == prev.Seq+1:
		return OrderNext
	default:
		return OrderGap
	}
}

// Violation is one stream-level contract breach found by ValidateStream.
type Violation struct {
	Index int    // index into the slice
	Seq   uint64 // seq of the offending event
	Rule  string // stable rule id (see the spec's §4 table)
	Msg   string
}

func (v Violation) String() string {
	return fmt.Sprintf("#%d seq=%d %s: %s", v.Index, v.Seq, v.Rule, v.Msg)
}

// Stream rule ids.
const (
	RuleEventInvalid   = "event-invalid"
	RuleSeqContiguous  = "seq-contiguous"
	RuleSeqDuplicate   = "seq-duplicate"
	RuleSession        = "single-session"
	RuleEpochMonotonic = "epoch-monotonic"
	RuleEpochStart     = "epoch-start"
	RuleMonoMonotonic  = "mono-monotonic"
	RuleAfterExit      = "no-io-after-exit"
	RuleGapMarker      = "gap-marker-covers-hole"
)

// ValidateStream checks a journal slice (as persisted, or as replayed to a
// client) against the ordering contract:
//
//   - every event is individually valid;
//   - one session; seq strictly contiguous, EXCEPT a hole immediately preceded
//     by a MarkerGap whose FromSeq..ToSeq covers exactly the missing range;
//   - epoch never decreases; the first event of each new epoch is a lifecycle
//     event whose phase StartsEpoch (spawned or recovered);
//   - MonoNS never decreases within an epoch (it restarts with the epoch);
//   - after a terminal lifecycle event (exited/signaled/crashed) no output or
//     input appears until the next epoch starts.
func ValidateStream(evs []Event) []Violation {
	var out []Violation
	add := func(i int, rule, format string, a ...any) {
		out = append(out, Violation{Index: i, Seq: evs[i].Seq, Rule: rule, Msg: fmt.Sprintf(format, a...)})
	}
	var gapTo uint64 // a pending MarkerGap's ToSeq, 0 when none
	ended := false
	for i, ev := range evs {
		if err := ev.Validate(); err != nil {
			add(i, RuleEventInvalid, "%v", err)
		}
		if i == 0 {
			if ev.Kind == KindMarker && ev.Marker != nil && ev.Marker.Type == MarkerGap {
				gapTo = ev.Marker.ToSeq
			}
			ended = ev.Kind == KindLifecycle && ev.Lifecycle != nil && ev.Lifecycle.Phase.Terminal()
			continue
		}
		prev := evs[i-1]
		if ev.SessionID != prev.SessionID {
			add(i, RuleSession, "session %q after %q", ev.SessionID, prev.SessionID)
		}
		switch {
		case ev.Seq <= prev.Seq:
			add(i, RuleSeqDuplicate, "seq %d after %d", ev.Seq, prev.Seq)
		case ev.Seq != prev.Seq+1:
			// A hole is legal only right after a gap marker that names it.
			if gapTo == 0 || prev.Seq+1 > gapTo || ev.Seq != gapTo+1 {
				add(i, RuleSeqContiguous, "seq %d after %d (missing %d..%d)", ev.Seq, prev.Seq, prev.Seq+1, ev.Seq-1)
			}
		}
		gapTo = 0
		if ev.Kind == KindMarker && ev.Marker != nil && ev.Marker.Type == MarkerGap {
			if ev.Marker.FromSeq != ev.Seq+1 || ev.Marker.ToSeq < ev.Marker.FromSeq {
				add(i, RuleGapMarker, "gap marker %d..%d must start at seq+1=%d", ev.Marker.FromSeq, ev.Marker.ToSeq, ev.Seq+1)
			} else {
				gapTo = ev.Marker.ToSeq
			}
		}
		switch {
		case ev.Epoch < prev.Epoch:
			add(i, RuleEpochMonotonic, "epoch %d after %d", ev.Epoch, prev.Epoch)
		case ev.Epoch > prev.Epoch:
			if ev.Kind != KindLifecycle || ev.Lifecycle == nil || !ev.Lifecycle.Phase.StartsEpoch() {
				add(i, RuleEpochStart, "epoch %d starts with %s, want lifecycle spawned|recovered", ev.Epoch, ev.Kind)
			}
			ended = false
		default:
			if ev.MonoNS < prev.MonoNS {
				add(i, RuleMonoMonotonic, "mono_ns %d after %d in epoch %d", ev.MonoNS, prev.MonoNS, ev.Epoch)
			}
		}
		if ended && (ev.Kind == KindOutput || ev.Kind == KindInput) {
			add(i, RuleAfterExit, "%s after the process ended in epoch %d", ev.Kind, ev.Epoch)
		}
		if ev.Kind == KindLifecycle && ev.Lifecycle != nil && ev.Lifecycle.Phase.Terminal() {
			ended = true
		}
	}
	return out
}

// Cursor is a client's resume position: the last Seq it applied for a session.
// It is opaque to the client apart from being echoed back on reconnect. Epoch
// is informational (Seq is session-global), but lets the server detect a client
// whose view predates a crash-recovery reset.
type Cursor struct {
	SessionID string `json:"session_id"`
	Epoch     uint64 `json:"epoch"`
	Seq       uint64 `json:"seq"`
}

// Window describes what the journal currently retains for a session.
// OldestSeq..NewestSeq are retained (inclusive); SnapshotSeq is the newest
// snapshot marker's ToSeq (0 = none); ResetSeq is the seq of the newest reset
// marker (0 = none) — a client positioned before it holds a screen the server
// can no longer vouch for.
type Window struct {
	SessionID   string
	Epoch       uint64
	OldestSeq   uint64
	NewestSeq   uint64
	SnapshotSeq uint64
	ResetSeq    uint64
}

// ResumeMode is how the server answers a reconnect.
type ResumeMode string

const (
	// ResumeTail: cursor is current; send live events from NewestSeq+1.
	ResumeTail ResumeMode = "tail"
	// ResumeReplay: replay FromSeq..NewestSeq from the journal, then tail.
	ResumeReplay ResumeMode = "replay"
	// ResumeSnapshot: the cursor fell out of retention (or behind a reset);
	// send the newest snapshot, then replay SnapshotSeq+1.., then tail. The
	// client must reset its terminal model first.
	ResumeSnapshot ResumeMode = "snapshot"
	// ResumeReject: the cursor names another session or lies in the future
	// (journal lost after a crash): client must discard state and attach fresh
	// (equivalent to ResumeSnapshot with a reset, but signalled distinctly so
	// clients can log it).
	ResumeReject ResumeMode = "reject"
)

// ResumePlan is PlanResume's answer.
type ResumePlan struct {
	Mode    ResumeMode
	FromSeq uint64 // first seq to send after any snapshot (0 when only tailing)
	Reason  string
}

// PlanResume decides how to resynchronize a reconnecting client. A zero Cursor
// (Seq 0, any session) is a fresh attach and always gets a snapshot when one
// exists, else a full replay from OldestSeq.
func PlanResume(c Cursor, w Window) ResumePlan {
	if c.Seq != 0 && c.SessionID != w.SessionID {
		return ResumePlan{Mode: ResumeReject, Reason: "cursor is for another session"}
	}
	if c.Seq > w.NewestSeq {
		return ResumePlan{Mode: ResumeReject, Reason: fmt.Sprintf("cursor seq %d is ahead of journal head %d", c.Seq, w.NewestSeq)}
	}
	fresh := c.Seq == 0
	behindReset := w.ResetSeq != 0 && c.Seq < w.ResetSeq
	outOfWindow := c.Seq+1 < w.OldestSeq
	if fresh || behindReset || outOfWindow {
		// A snapshot is usable only when the events after it are still retained.
		if w.SnapshotSeq != 0 && w.SnapshotSeq+1 >= w.OldestSeq {
			reason := "fresh attach"
			switch {
			case behindReset && !fresh:
				reason = "cursor predates a terminal reset"
			case outOfWindow && !fresh:
				reason = "cursor fell out of retention"
			}
			return ResumePlan{Mode: ResumeSnapshot, FromSeq: w.SnapshotSeq + 1, Reason: reason}
		}
		if fresh {
			return ResumePlan{Mode: ResumeReplay, FromSeq: w.OldestSeq, Reason: "fresh attach, no snapshot"}
		}
		return ResumePlan{Mode: ResumeReject, Reason: "cursor unrecoverable and no snapshot retained"}
	}
	if c.Seq == w.NewestSeq {
		return ResumePlan{Mode: ResumeTail, Reason: "up to date"}
	}
	return ResumePlan{Mode: ResumeReplay, FromSeq: c.Seq + 1, Reason: "replay missed events"}
}
