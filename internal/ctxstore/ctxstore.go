// Package ctxstore is a daemon-owned, namespaced key/value store that agents
// read and write to share results and state (the "shared context" / blackboard).
// Keys are free-form dot-namespaced strings (e.g. "global.foo",
// "pipeline.<pid>.<job>.output").
//
// Entries are persisted as records in an embedded ScrivaDB "context" collection
// (github.com/srjn45/scriva), one append-only NDJSON collection under the data
// dir — each key is a record keyed by the context key, so a write appends a
// single record instead of rewriting a whole-store map. The collection is opened
// with SyncModeNone: like the previous single-file implementation this is a
// localhost session store, so the last write surviving a power-loss is not a
// requirement (append-only segments rule out torn reads regardless).
//
// The `updated_by` field is advisory provenance, not an authenticated identity:
// warden assumes a single trusted local user, so callers must not make security
// decisions on it. The daemon edge (daemon.sanitizeSender) reserves the
// "daemon"/"system" ids so an agent can't forge daemon-originated writes.
package ctxstore

import (
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("context key not found")

// ErrBadKey is returned for an empty/blank key or one containing a path
// separator.
var ErrBadKey = errors.New("invalid context key")

// ErrConflict is returned by CompareAndSet when the current value does not match
// the caller's expected value — another writer won the race.
var ErrConflict = errors.New("context value conflict")

// record data field names. The caller's context key is stored separately as the
// ScrivaDB record key (engine.KeyField), so it round-trips through Scan and Watch.
const (
	fieldValue = "value"
	fieldBy    = "by"
	fieldAt    = "at"
)

// validKey rejects empty/blank keys and keys containing a path separator. Keys
// are dot-namespaced strings that travel through a URL path segment; a "/" is
// not decoded back by the router, so it would be stored under a corrupted key
// and break prefix operations (e.g. pipeline-scoped cleanup). Reject it at the
// single write gate.
func validKey(key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	return !strings.ContainsAny(key, `/\`)
}

// Entry is one stored value plus its provenance.
type Entry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists all entries in an embedded ScrivaDB "context" collection.
type Store struct {
	db  *scriva.DB
	col *engine.Collection
}

// New creates dir (if needed) and returns a store backed by a ScrivaDB "context"
// collection rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("context")
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// data builds a record body for value/by at the given time.
func data(value, by string, at time.Time) map[string]any {
	return map[string]any{
		fieldValue: value,
		fieldBy:    by,
		fieldAt:    at.Format(time.RFC3339Nano),
	}
}

// toEntry reconstructs an Entry from a record's key and data.
func toEntry(key string, d map[string]any) Entry {
	e := Entry{Key: key}
	e.Value, _ = d[fieldValue].(string)
	e.UpdatedBy, _ = d[fieldBy].(string)
	if s, ok := d[fieldAt].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			e.UpdatedAt = t
		}
	}
	return e
}

// Set writes value at key, recording the writer (by) and current time.
func (s *Store) Set(key, value, by string) (Entry, error) {
	if !validKey(key) {
		return Entry{}, ErrBadKey
	}
	at := time.Now().UTC()
	if _, err := s.col.Upsert(key, data(value, by, at)); err != nil {
		return Entry{}, err
	}
	return Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: at}, nil
}

// CompareAndSet writes value at key only if the current value equals expected
// (expected "" means "the key must be absent"). On a mismatch it makes no change
// and returns ErrConflict, so an agent doing read-modify-write on the shared
// blackboard can re-read and retry instead of silently losing a concurrent
// writer's update — the atomic alternative to Get-then-Set. The absent-key case
// is an InsertWithKey (rejected with ErrDuplicateKey if the key already exists);
// the value-match case is an engine compare-and-swap (UpdateIfMatch).
func (s *Store) CompareAndSet(key, expected, value, by string) (Entry, error) {
	if !validKey(key) {
		return Entry{}, ErrBadKey
	}
	at := time.Now().UTC()
	body := data(value, by, at)
	if expected == "" {
		_, _, err := s.col.InsertWithKey(key, body)
		if errors.Is(err, engine.ErrDuplicateKey) {
			return Entry{}, ErrConflict
		}
		if err != nil {
			return Entry{}, err
		}
		return Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: at}, nil
	}
	applied, err := s.col.UpdateIfMatch(key, func(cur map[string]any) bool {
		v, _ := cur[fieldValue].(string)
		return v == expected
	}, body)
	if err != nil {
		return Entry{}, err
	}
	if !applied {
		return Entry{}, ErrConflict
	}
	return Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: at}, nil
}

// Append atomically sets key to its current value + sep + value, creating the
// key (with no leading sep) when absent. It is the race-free form of the common
// "accumulate into a shared key" pattern that a Get-then-Set would corrupt under
// concurrent writers. It read-modify-writes under an engine compare-and-swap and
// retries on a lost race, so it is correct against any concurrent writer without
// a store-wide lock.
func (s *Store) Append(key, value, sep, by string) (Entry, error) {
	if !validKey(key) {
		return Entry{}, ErrBadKey
	}
	for {
		at := time.Now().UTC()
		r, err := s.col.GetByKey(key)
		if errors.Is(err, engine.ErrKeyNotFound) {
			// Absent: try to create it. If another writer created it first, retry
			// as an update rather than clobbering their value.
			_, _, err := s.col.InsertWithKey(key, data(value, by, at))
			if errors.Is(err, engine.ErrDuplicateKey) {
				continue
			}
			if err != nil {
				return Entry{}, err
			}
			return Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: at}, nil
		}
		if err != nil {
			return Entry{}, err
		}
		cur, _ := r.Data[fieldValue].(string)
		next := cur + sep + value
		applied, err := s.col.UpdateIfRev(key, r.Rev, data(next, by, at))
		if err != nil {
			return Entry{}, err
		}
		if applied {
			return Entry{Key: key, Value: next, UpdatedBy: by, UpdatedAt: at}, nil
		}
		// Lost the race — the record changed under us; re-read and retry.
	}
}

// Get returns the entry at key, or ErrNotFound.
func (s *Store) Get(key string) (Entry, error) {
	r, err := s.col.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	return toEntry(key, r.Data), nil
}

// List returns all entries whose key starts with prefix (empty = all), sorted
// by key. Always returns a non-nil slice.
func (s *Store) List(prefix string) ([]Entry, error) {
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, r := range results {
		key, _ := r.Data[engine.KeyField].(string)
		if prefix == "" || strings.HasPrefix(key, prefix) {
			out = append(out, toEntry(key, r.Data))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// DelPrefix removes every key starting with prefix and returns how many were
// removed. An empty prefix is rejected (ErrBadKey) so a caller can never wipe
// the whole store by accident — this backs pipeline-scoped cleanup, where the
// caller passes "pipeline.<id>." (trailing dot included so "p1" can't match
// "p10"). A no-match prefix is not an error; it returns (0, nil).
func (s *Store) DelPrefix(prefix string) (int, error) {
	if prefix == "" {
		return 0, ErrBadKey
	}
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range results {
		key, _ := r.Data[engine.KeyField].(string)
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if err := s.col.DeleteByKey(key); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// Del removes key, returning ErrNotFound if it was absent.
func (s *Store) Del(key string) error {
	err := s.col.DeleteByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// Close flushes and releases the underlying ScrivaDB collection (stopping its
// background compaction goroutine). The daemon calls it on shutdown.
func (s *Store) Close() error {
	return s.db.Close()
}
