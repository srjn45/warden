// Package mailbox is a daemon-owned per-recipient message store — the durable
// inbox behind agent-to-agent directed messages. Each recipient's messages live
// in one JSON file (<dir>/<id>.json), rewritten atomically (temp file + rename)
// under a mutex. Localhost session-store scale, like internal/ctxstore.
package mailbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// ErrBadRecipient is returned when a recipient id is unsafe as a filename.
var ErrBadRecipient = errors.New("invalid recipient")

// Message is one directed message in a recipient's inbox.
type Message struct {
	ID   string    `json:"id"`   // per-inbox sequence, 1-based
	From string    `json:"from"` // sender id, or "human"/"daemon"
	To   string    `json:"to"`
	Body string    `json:"body"`
	TS   time.Time `json:"ts"`
	Read bool      `json:"read"`
}

// Store persists each recipient's messages in its own JSON file.
type Store struct {
	mu  sync.Mutex
	dir string
}

// New creates dir (if needed) and returns a ready store.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// path maps a recipient id to its inbox file, rejecting unsafe ids.
func (s *Store) path(to string) (string, error) {
	if err := store.SafeID(to); err != nil {
		return "", ErrBadRecipient
	}
	return filepath.Join(s.dir, to+".json"), nil
}

// load reads a recipient's messages; a missing file is an empty slice.
func (s *Store) load(path string) ([]Message, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Message{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ms []Message
	if err := json.Unmarshal(data, &ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// save writes messages via temp file + rename so readers never see a partial file.
func (s *Store) save(path string, ms []Message) error {
	data, err := json.MarshalIndent(ms, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Append stores m in m.To's inbox, assigning a per-inbox sequential ID and TS.
func (s *Store) Append(m Message) (Message, error) {
	path, err := s.path(m.To)
	if err != nil {
		return Message{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.load(path)
	if err != nil {
		return Message{}, err
	}
	m.ID = strconv.Itoa(len(ms) + 1)
	m.TS = time.Now().UTC()
	m.Read = false
	ms = append(ms, m)
	if err := s.save(path, ms); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Messages returns to's inbox in arrival order (read-only). Always non-nil.
func (s *Store) Messages(to string) ([]Message, error) {
	path, err := s.path(to)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(path)
}

// All returns every message across all recipients' inboxes (read-only, no
// mark-read), in unspecified order. Backs the daemon's global, read-only
// message-traffic view. Temp files from in-flight atomic writes (.tmp-*) and any
// non-.json entry are skipped; a single corrupt inbox aborts with its error.
func (s *Store) All() ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		ms, err := s.load(filepath.Join(s.dir, ent.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// DeleteInbox removes a recipient's entire inbox file. A missing file is a
// no-op (nil) — inboxes are created lazily, so "never had one" and "had one,
// now cleared" are the same end state. Backs cleanup when an agent is
// hard-deleted; safe because nothing (pipelines included) reads another agent's
// inbox to make progress.
func (s *Store) DeleteInbox(to string) error {
	path, err := s.path(to)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// MarkRead flags the given message IDs read in to's inbox. Unknown IDs are
// ignored; a no-op (nothing changed) avoids a rewrite.
func (s *Store) MarkRead(to string, ids []string) error {
	path, err := s.path(to)
	if err != nil {
		return err
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.load(path)
	if err != nil {
		return err
	}
	changed := false
	for i := range ms {
		if want[ms[i].ID] && !ms[i].Read {
			ms[i].Read = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save(path, ms)
}

// TakeFirstUnread atomically finds the oldest unread message in to's inbox
// matching from ("" = any sender), marks it read, and returns it. ok is false
// when nothing matches.
func (s *Store) TakeFirstUnread(to, from string) (Message, bool, error) {
	path, err := s.path(to)
	if err != nil {
		return Message{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.load(path)
	if err != nil {
		return Message{}, false, err
	}
	for i := range ms {
		if ms[i].Read {
			continue
		}
		if from != "" && ms[i].From != from {
			continue
		}
		ms[i].Read = true
		if err := s.save(path, ms); err != nil {
			return Message{}, false, err
		}
		return ms[i], true, nil
	}
	return Message{}, false, nil
}
