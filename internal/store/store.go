package store

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a session id does not exist.
var ErrNotFound = errors.New("session not found")

// ErrExists is returned when inserting a session id that already exists.
var ErrExists = errors.New("session already exists")

// Store is the persistence boundary. Only the daemon holds a Store.
type Store interface {
	Insert(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)
	UpdateStatus(ctx context.Context, id string, status Status) error
	// UpdateStatusIf is a compare-and-swap: it sets status to next only if the
	// stored status still equals expected, reporting whether the swap happened.
	// The poller uses it so a status derived from a stale snapshot can't clobber
	// a newer status written by a hook between the poller's List and its write.
	UpdateStatusIf(ctx context.Context, id string, expected, next Status) (bool, error)
	UpdateType(ctx context.Context, id string, t Type) error
	UpdateSubject(ctx context.Context, id, subject string) error
	AppendEvent(ctx context.Context, id string, ev Event) error
	// AppendEventStatus appends ev and, when status is non-empty, sets status —
	// in a single atomic update. The hooks endpoint uses it so an event and its
	// status transition can never land half-applied.
	AppendEventStatus(ctx context.Context, id string, ev Event, status Status) error
	UpdatePane(ctx context.Context, id, excerpt string) error
	// ClearWorktree blanks the Worktree and Branch fields (after the worktree is
	// removed from disk), so the record no longer points at a gone worktree.
	ClearWorktree(ctx context.Context, id string) error
	// Archive moves the doc to the closed collection (soft delete).
	Archive(ctx context.Context, id string) error
	// Delete hard-removes the doc.
	Delete(ctx context.Context, id string) error
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}
