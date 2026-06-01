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
	AppendEvent(ctx context.Context, id string, ev Event) error
	UpdatePane(ctx context.Context, id, excerpt string) error
	// Archive moves the doc to the closed collection (soft delete).
	Archive(ctx context.Context, id string) error
	// Delete hard-removes the doc.
	Delete(ctx context.Context, id string) error
	Ping(ctx context.Context) error
	Close(ctx context.Context) error
}
