package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a session id does not exist.
var ErrNotFound = errors.New("session not found")

// ErrExists is returned when inserting a session id that already exists.
var ErrExists = errors.New("session already exists")

// ErrNameExists is returned when inserting a session with a name that already exists.
var ErrNameExists = errors.New("agent name already exists")

// ErrInvalidName is returned when a session name is invalid.
var ErrInvalidName = errors.New("invalid agent name: must be 1-32 alphanumeric chars, hyphens, or underscores")

// ErrStoreOwned is returned by NewFileStore when the data directory's writable
// session store is already held by another live process (normally the daemon).
// A single process — the daemon — is the only legitimate writer; a second
// writable opener (a stray CLI hot-swap, an offline repair run while the daemon
// is up) must fail deterministically here rather than corrupt the shared
// append-only segments. The exclusion is enforced by an advisory lock the OS
// releases automatically on process death, so a crashed owner never leaves a
// permanently stuck store.
var ErrStoreOwned = errors.New("session store already owned by another process (is the daemon running?)")

// DegradationClass classifies why an active-collection scan could not return the
// complete fleet. It is stable, machine-readable diagnostic data for operator
// tooling and the store-health endpoint.
type DegradationClass string

const (
	// DegradeDecode: a record's stored payload would not decode into a Session
	// (shape-invalid / schema drift). The engine returned the record but its body
	// is unusable.
	DegradeDecode DegradationClass = "decode"
	// DegradeRead: the underlying engine scan itself failed (segment framing,
	// checksum, or index read error) before any record could be examined.
	DegradeRead DegradationClass = "read"
)

// ScanFailure is one record (or one whole-scan) failure recorded during an
// active-collection scan. Key is the session id when the engine surfaced the
// record; it is empty for a DegradeRead failure that aborted the scan wholesale.
type ScanFailure struct {
	Collection string           `json:"collection"`
	Key        string           `json:"key,omitempty"`
	Class      DegradationClass `json:"class"`
	Detail     string           `json:"detail"`
}

// DegradedScanError is returned by an active-collection read (List,
// GetByNameOrID's name scan, Insert's uniqueness scan) when one or more records
// could not be read. It signals the caller that the fleet snapshot is
// UNAVAILABLE — never a silently short list — so REST/SSE consumers can degrade
// explicitly instead of publishing a partial fleet. It carries structured
// per-failure diagnostics for the store-health endpoint and offline repair.
type DegradedScanError struct {
	Failures []ScanFailure
}

func (e *DegradedScanError) Error() string {
	if e == nil || len(e.Failures) == 0 {
		return "session store degraded"
	}
	parts := make([]string, 0, len(e.Failures))
	for _, f := range e.Failures {
		if f.Key != "" {
			parts = append(parts, fmt.Sprintf("%s/%s: %s (%s)", f.Collection, f.Key, f.Detail, f.Class))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %s (%s)", f.Collection, f.Detail, f.Class))
		}
	}
	return fmt.Sprintf("session store degraded: %d unreadable record(s): %s", len(e.Failures), strings.Join(parts, "; "))
}

// IsDegraded reports whether err is (or wraps) a DegradedScanError, and returns
// the typed value for its diagnostics. Callers use it to map a degraded active
// read to an explicit non-success response.
func IsDegraded(err error) (*DegradedScanError, bool) {
	return errors.AsType[*DegradedScanError](err)
}

// Store is the persistence boundary. Only the daemon holds a Store.
type Store interface {
	Insert(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	// GetByNameOrID looks up a session by name first (exact case-sensitive match
	// among active sessions), falling back to ID lookup if no name matches.
	// Returns ErrNotFound if neither name nor ID match any active session.
	GetByNameOrID(ctx context.Context, nameOrID string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)
	// ListClosed returns all archived (closed) sessions. Archived records still
	// legitimately own their worktree, so prune consults them before reclaiming.
	ListClosed(ctx context.Context) ([]*Session, error)
	// Update is the general transactional read-modify-write primitive: it loads the
	// active session, invokes fn with a pointer to the current in-memory record, and
	// (unless fn returns an error, which aborts the whole update leaving the stored
	// session unchanged) bumps UpdatedAt and writes it back — the whole cycle atomic
	// under the store's per-store lock. Prefer it for any single-/dual-field mutation
	// rather than growing a new bespoke setter on this interface. A missing session
	// returns ErrNotFound. CAS-style callers that must key off the current status and
	// report whether a swap happened still use UpdateStatusIf/FinalizeExit.
	Update(ctx context.Context, id string, fn func(s *Session) error) error
	UpdateStatus(ctx context.Context, id string, status Status) error
	// UpdateStatusIf is a compare-and-swap: it sets status to next only if the
	// stored status still equals expected, reporting whether the swap happened.
	// The poller uses it so a status derived from a stale snapshot can't clobber
	// a newer status written by a hook between the poller's List and its write.
	UpdateStatusIf(ctx context.Context, id string, expected, next Status) (bool, error)
	// FinalizeExit is a compare-and-swap like UpdateStatusIf that also records the
	// process exit code and, for a non-zero code, appends a "session exited" event
	// — all in one atomic write. The poller uses it to finalize an agent from its
	// exit-file without clobbering a status a SessionEnd hook already set.
	FinalizeExit(ctx context.Context, id string, expected, next Status, code int) (bool, error)
	// SetSessionID pins the backend session id (ClaudeSessionID) for a session.
	// Used by the poller's discover-then-pin path: a non-pinning backend mints its
	// own id at launch, which warden discovers post-launch and persists here so the
	// transcript path + resume key off the exact id instead of dir-scoping.
	SetSessionID(ctx context.Context, id, sessionID string) error
	AppendEvent(ctx context.Context, id string, ev Event) error
	// AppendEventStatus appends ev and, when status is non-empty, sets status —
	// in a single atomic update. The hooks endpoint uses it so an event and its
	// status transition can never land half-applied.
	AppendEventStatus(ctx context.Context, id string, ev Event, status Status) error
	// SetRestart records an auto-restart attempt's counter and timestamp.
	SetRestart(ctx context.Context, id string, count int, at time.Time) error
	// UpdateContext persists the context-window gauge (tokens + state band),
	// appending a "context" event only on a state transition.
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	// StampCompact records the time of an auto-/compact (cooldown guard).
	StampCompact(ctx context.Context, id string) error
	// UpdateAutoApprove sets the AutoApprove flag for a session.
	UpdateAutoApprove(ctx context.Context, id string, enabled bool) error
	// SetForceCompact sets the per-agent force-compact override (nil = inherit the
	// global token_force_compact; non-nil pins the agent on/off).
	SetForceCompact(ctx context.Context, id string, v *bool) error
	// UpdatePermissionMode sets the permission mode for a session.
	UpdatePermissionMode(ctx context.Context, id string, mode string) error
	// UpdateRole sets the built-in role name for a session (empty = "general").
	// The persona is re-resolved from the registry at launch, so only the name is
	// stored; switching a role + resuming re-injects the new persona.
	UpdateRole(ctx context.Context, id string, role string) error
	// ClearWorktree blanks the Worktree and Branch fields (after the worktree is
	// removed from disk), so the record no longer points at a gone worktree.
	ClearWorktree(ctx context.Context, id string) error
	// SetRateLimit records rate limit state and next resume time.
	SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error
	// ClearRateLimit removes rate limit metadata (after successful resume).
	ClearRateLimit(ctx context.Context, id string) error
	// Archive moves the doc to the closed collection (soft delete).
	Archive(ctx context.Context, id string) error
	// Delete hard-removes the doc.
	Delete(ctx context.Context, id string) error
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}

// ArchiveDegradationReader is implemented by stores that can report tolerant
// archive decode skips. Callers presenting history/export data use it to avoid
// describing an incomplete archive as complete without expanding Store for
// lightweight test and plugin implementations.
type ArchiveDegradationReader interface {
	ListClosedDegraded(ctx context.Context) ([]*Session, int, error)
}
