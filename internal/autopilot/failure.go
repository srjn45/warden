package autopilot

import (
	"errors"
	"strings"

	"github.com/srjn45/warden/internal/router"
)

// FailureKind classifies why the guardian could not (re)spawn a manager brain,
// so a backoff reports the real cause instead of always blaming rate limits.
type FailureKind string

const (
	// KindBackendUnavailable: the chosen backend is unavailable or rate-limited
	// (transient — a later retry may succeed).
	KindBackendUnavailable FailureKind = "backend_unavailable"
	// KindNoBackendSelectable: backend selection returned nothing selectable
	// (transient; includes the gate-only "pay-per-use" case).
	KindNoBackendSelectable FailureKind = "no_backend_selectable"
	// KindDefinitionError: the plan could not be loaded or the digest could not
	// be composed — retrying cannot help until the definition is fixed.
	KindDefinitionError FailureKind = "definition_error"
	// KindSpawnError: any other spawn/rotate failure (unknown cause).
	KindSpawnError FailureKind = "spawn_error"
)

// ErrBackendUnavailable is the sentinel runtimes may wrap to flag a backend as
// unavailable or rate-limited at spawn time.
var ErrBackendUnavailable = errors.New("backend unavailable")

// SpawnFailure is a spawn/rotate error tagged with its kind.
type SpawnFailure struct {
	Kind FailureKind
	Err  error
}

func (e *SpawnFailure) Error() string { return e.Err.Error() }
func (e *SpawnFailure) Unwrap() error { return e.Err }

func tagFailure(kind FailureKind, err error) error {
	if err == nil {
		return nil
	}
	return &SpawnFailure{Kind: kind, Err: err}
}

// classifySpawnError returns the failure kind for a spawn/rotate error: an
// explicit SpawnFailure tag wins, then the backend-unavailable sentinels, then a
// conservative substring check for well-known runtime messages; else unknown.
func classifySpawnError(err error) FailureKind {
	var sf *SpawnFailure
	if errors.As(err, &sf) {
		return sf.Kind
	}
	if errors.Is(err, ErrBackendUnavailable) || errors.Is(err, router.ErrAllExhausted) {
		return KindBackendUnavailable
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"rate limit", "rate-limit", "usage limit", "backend unavailable", "unavailable"} {
		if strings.Contains(msg, s) {
			return KindBackendUnavailable
		}
	}
	return KindSpawnError
}
