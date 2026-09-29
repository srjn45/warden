package autopilot

import "sort"

// preflightFailureKind classifies a preflight failure as structural (the plan
// cannot run at all) or content (a transient or fixable data issue that boot
// recovery can work around with a lenient plan load).
type preflightFailureKind int

const (
	preflightKindStructural preflightFailureKind = iota
	preflightKindContent
)

// preflightFailure carries one human-readable failure message and its kind.
type preflightFailure struct {
	msg  string
	kind preflightFailureKind
}

// newPreflightError builds a *PreflightError from a typed slice, populating
// both the wire-format Failures []string and the internal kinds
// []preflightFailureKind in lock-step so callers can query hasStructural /
// hasContentOnly. The input slice is sorted by message and deduplicated.
func newPreflightError(failures []preflightFailure) *PreflightError {
	if len(failures) == 0 {
		return &PreflightError{}
	}
	sorted := make([]preflightFailure, len(failures))
	copy(sorted, failures)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].msg < sorted[j].msg })
	// Deduplicate adjacent equal messages (keep first kind seen).
	deduped := sorted[:1]
	for i := 1; i < len(sorted); i++ {
		if sorted[i].msg != sorted[i-1].msg {
			deduped = append(deduped, sorted[i])
		}
	}
	e := &PreflightError{
		Failures: make([]string, len(deduped)),
		kinds:    make([]preflightFailureKind, len(deduped)),
	}
	for i, f := range deduped {
		e.Failures[i] = f.msg
		e.kinds[i] = f.kind
	}
	return e
}

// hasStructural reports whether any failure in e is structural (the plan is
// unrunnable regardless of transient state).
func (e *PreflightError) hasStructural() bool {
	for _, k := range e.kinds {
		if k == preflightKindStructural {
			return true
		}
	}
	return false
}

// hasContentOnly reports whether e has at least one failure and ALL failures
// are content-kind (transient or fixable at runtime via lenient load).
func (e *PreflightError) hasContentOnly() bool {
	return len(e.kinds) > 0 && !e.hasStructural()
}
