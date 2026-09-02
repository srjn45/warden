package autopilot

import (
	"errors"
	"fmt"
	"strings"
)

// LedgerState is a canonical task-ledger state (autopilot.md §2.2). It is
// distinct from plan-file checklist statuses on PlanTask (pending/active/done/
// failed): the ledger tracks live worker progress; the plan file is the owner's
// coarse checklist.
type LedgerState string

const (
	LedgerPending    LedgerState = "pending"
	LedgerAssigned   LedgerState = "assigned"
	LedgerInProgress LedgerState = "in_progress"
	LedgerPROpen     LedgerState = "pr_open"
	LedgerGated      LedgerState = "gated"
	LedgerLanded     LedgerState = "landed"
)

// CanonicalLedgerStates is the TUI segmentation order: pending → assigned →
// in_progress → pr_open → gated → landed. Extra spec-machine labels such as
// "fixing" and "replanned" are not stored values; illegal-transition checks are
// deferred to autopilot semantics owners.
var CanonicalLedgerStates = []LedgerState{
	LedgerPending,
	LedgerAssigned,
	LedgerInProgress,
	LedgerPROpen,
	LedgerGated,
	LedgerLanded,
}

// ErrInvalidLedgerState is returned when a ledger write carries a state outside
// the canonical set.
var ErrInvalidLedgerState = errors.New("autopilot: invalid ledger task state")

// ErrInvalidLedgerTask is returned when a ledger write row is missing its id.
var ErrInvalidLedgerTask = errors.New("autopilot: invalid ledger task")

func (s LedgerState) String() string { return string(s) }

// Valid reports whether s is one of the canonical write states.
func (s LedgerState) Valid() bool {
	switch s {
	case LedgerPending, LedgerAssigned, LedgerInProgress, LedgerPROpen, LedgerGated, LedgerLanded:
		return true
	}
	return false
}

// SegmentIndex is s's position in CanonicalLedgerStates, or -1 if s is not a
// TUI segment (unknown / not yet written). Worker groups render in this order.
func (s LedgerState) SegmentIndex() int {
	for i, c := range CanonicalLedgerStates {
		if s == c {
			return i
		}
	}
	return -1
}

// ParseLedgerState maps a wire/ctx token onto a canonical state. Matching is
// case-insensitive and trims space; anything else is ErrInvalidLedgerState.
func ParseLedgerState(s string) (LedgerState, error) {
	st := LedgerState(strings.ToLower(strings.TrimSpace(s)))
	if !st.Valid() {
		return "", invalidLedgerStateError(st)
	}
	return st, nil
}

func invalidLedgerStateError(state LedgerState) error {
	return fmt.Errorf("%w %q (want pending, assigned, in_progress, pr_open, gated, or landed)", ErrInvalidLedgerState, state)
}

func validateLedgerTasks(tasks []LedgerTask) error {
	for i, t := range tasks {
		if strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("%w: task[%d] has an empty id", ErrInvalidLedgerTask, i)
		}
		if !t.State.Valid() {
			return fmt.Errorf("%w: task %q", invalidLedgerStateError(t.State), t.ID)
		}
	}
	return nil
}
