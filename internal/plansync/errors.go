package plansync

import (
	"errors"
	"fmt"
)

var (
	// ErrConflict is returned by providers that enforce ConflictToken when a
	// Push races an existing remote (or fake) revision.
	ErrConflict = errors.New("plansync: conflict token mismatch")
	// ErrDisabled is reserved for callers that require an enabled Hub provider.
	ErrDisabled = errors.New("plansync: provider disabled")
)

func errSchema(v int) error {
	return fmt.Errorf("plansync: unsupported schema_version %d (want %d)", v, SchemaVersion)
}

func errMissing(field string) error {
	return fmt.Errorf("plansync: %s is required", field)
}

func errInvalidVisibility(v Visibility) error {
	return fmt.Errorf("plansync: invalid visibility %q", v)
}

// ConflictError is the structured conflict a future Hub (and FakeProvider)
// returns when ConflictToken does not match the stored revision.
type ConflictError struct {
	PlanID   string
	Expected string
	Actual   string
}

func (e *ConflictError) Error() string {
	if e == nil {
		return ErrConflict.Error()
	}
	return fmt.Sprintf("plansync: plan %s conflict: expected token %q, actual %q", e.PlanID, e.Expected, e.Actual)
}

func (e *ConflictError) Unwrap() error { return ErrConflict }
