package ptyevent

import (
	"fmt"
	"time"
)

// RevalidationError is why a decision must not be delivered. Code is the
// stable string recorded in DecisionEvent.Revalidation.
type RevalidationError struct {
	Code string
	Msg  string
}

func (e *RevalidationError) Error() string { return e.Code + ": " + e.Msg }

// Revalidation codes.
const (
	RevalOK              = "ok"
	RevalPromptGone      = "prompt-gone"
	RevalPromptChanged   = "prompt-changed"
	RevalTargetChanged   = "target-changed"
	RevalOptionMoved     = "option-moved"
	RevalHumanIntervened = "human-intervened"
	RevalStale           = "stale"
	RevalStateChanged    = "state-changed"
	RevalNotActionable   = "not-actionable"
)

// DefaultMaxDecisionAge bounds how old a decision may be when delivered.
const DefaultMaxDecisionAge = 30 * time.Second

// PendingDecision is a decision captured against one prompt observation.
type PendingDecision struct {
	Decision   Decision
	Prompt     Prompt // the prompt as decided (a copy)
	DecidedSeq uint64 // journal head when decided
	DecidedAt  time.Time
}

// Live is the runtime's view immediately before writing the keystrokes.
type Live struct {
	Prompt   *Prompt // currently on screen (nil = no prompt)
	State    State
	HumanSeq uint64 // Lease.HumanSeq: last human input seq
	Now      time.Time
}

// Revalidate re-checks a pending decision against the live screen right
// before delivery (time-of-check/time-of-use). Every condition must hold:
//
//   - the decision is approve/deny (others write nothing);
//   - the session is still waiting_for_permission;
//   - a prompt is on screen with the same Fingerprint AND InstanceKey (the
//     same shape asking about the same subject+target — a different command
//     behind an identical-looking box fails here);
//   - the chosen option still exists at the same index with the same label
//     and semantic;
//   - (navigate menus: delivery recomputes the cursor moves from the LIVE
//     prompt's Default, never from the decided copy);
//   - no human input since the decision (HumanSeq ≤ DecidedSeq);
//   - the decision is younger than maxAge.
//
// A failure means: drop the decision and re-decide from fresh evidence. It is
// never retried blindly.
func Revalidate(pd PendingDecision, live Live, maxAge time.Duration) error {
	fail := func(code, format string, a ...any) error {
		return &RevalidationError{Code: code, Msg: fmt.Sprintf(format, a...)}
	}
	act := pd.Decision.Action
	if act != ActionApprove && act != ActionDeny {
		return fail(RevalNotActionable, "%s writes no keystrokes", act)
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxDecisionAge
	}
	if age := live.Now.Sub(pd.DecidedAt); age > maxAge {
		return fail(RevalStale, "decided %s ago (max %s)", age, maxAge)
	}
	if live.HumanSeq > pd.DecidedSeq {
		return fail(RevalHumanIntervened, "human input at seq %d after decision at seq %d", live.HumanSeq, pd.DecidedSeq)
	}
	if live.State != StateWaitingForPermission {
		return fail(RevalStateChanged, "state is %s", live.State)
	}
	if live.Prompt == nil {
		return fail(RevalPromptGone, "no prompt on screen")
	}
	if live.Prompt.Fingerprint != pd.Prompt.Fingerprint {
		return fail(RevalPromptChanged, "fingerprint %s != %s", live.Prompt.Fingerprint, pd.Prompt.Fingerprint)
	}
	if live.Prompt.InstanceKey != pd.Prompt.InstanceKey || live.Prompt.Target != pd.Prompt.Target || live.Prompt.Subject != pd.Prompt.Subject {
		return fail(RevalTargetChanged, "same shape, different subject/target")
	}
	idx := pd.Decision.OptionIndex
	if idx < 1 || idx > len(live.Prompt.Options) || idx > len(pd.Prompt.Options) {
		return fail(RevalOptionMoved, "option %d no longer exists", idx)
	}
	was, now := pd.Prompt.Options[idx-1], live.Prompt.Options[idx-1]
	if was.Label != now.Label || was.Semantic != now.Semantic {
		return fail(RevalOptionMoved, "option %d was %q/%s, now %q/%s", idx, was.Label, was.Semantic, now.Label, now.Semantic)
	}
	return nil
}
