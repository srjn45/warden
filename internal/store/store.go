package store

import (
	"context"
	"errors"
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
	UpdateType(ctx context.Context, id string, t Type) error
	UpdateSubject(ctx context.Context, id, subject string) error
	AppendEvent(ctx context.Context, id string, ev Event) error
	// AppendEventStatus appends ev and, when status is non-empty, sets status —
	// in a single atomic update. The hooks endpoint uses it so an event and its
	// status transition can never land half-applied.
	AppendEventStatus(ctx context.Context, id string, ev Event, status Status) error
	UpdatePane(ctx context.Context, id, excerpt string) error
	// SetRestart records an auto-restart attempt's counter and timestamp.
	SetRestart(ctx context.Context, id string, count int, at time.Time) error
	// UpdateContext persists the context-window gauge (tokens + state band),
	// appending a "context" event only on a state transition.
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	// StampCompact records the time of an auto-/compact (cooldown guard).
	StampCompact(ctx context.Context, id string) error
	// UpdateAutoApprove sets the AutoApprove flag for a session.
	UpdateAutoApprove(ctx context.Context, id string, enabled bool) error
	// UpdatePermissionMode sets the permission mode for a session.
	UpdatePermissionMode(ctx context.Context, id string, mode string) error
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
