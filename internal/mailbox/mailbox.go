// Package mailbox is a daemon-owned per-recipient message store — the durable
// inbox behind agent-to-agent directed messages.
//
// Messages are persisted as records in an embedded FileDB "messages" collection
// (github.com/srjn45/filedbv2): one append-only NDJSON collection under the data
// dir, each record keyed by "<to>:<id>" with a secondary index on the recipient
// ("to") field for O(matches) per-inbox lookup. Appending a message writes a
// single record instead of rewriting a recipient's whole inbox file. The
// collection is opened with SyncModeNone: like the previous per-file
// implementation this is a localhost session store, so the last write surviving
// a power-loss is not a requirement (append-only segments rule out torn reads).
//
// Compound operations (Append and its compaction, MarkRead, TakeFirstUnread,
// DeleteInbox) are serialised by a store mutex, matching the previous
// single-mutex model; the per-inbox message id remains a high-water mark so
// compaction never recycles an id still referenced by a client or a MarkRead.
//
// The `from` field is advisory provenance, not an authenticated identity: warden
// assumes a single trusted local user, so callers must not make security
// decisions on it. The daemon edge (daemon.sanitizeSender) reserves the
// "daemon"/"system" ids so an agent can't forge daemon-originated messages.
package mailbox

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/srjn45/filedbv2/engine"
	"github.com/srjn45/filedbv2/filedb"
	"github.com/srjn45/filedbv2/query"
	"github.com/srjn45/warden/internal/store"
)

// ErrBadRecipient is returned when a recipient id is unsafe.
var ErrBadRecipient = errors.New("invalid recipient")

// toField is the secondary-indexed record field carrying the recipient id.
const toField = "to"

// retention bounds on a single inbox. An inbox is only ever appended to or
// marked-read; without bounds a long-lived agent's inbox grows without limit and
// each per-inbox scan gets slower. Append compacts to these limits; unread
// messages are never dropped.
const (
	// maxInboxMessages caps total retained messages; the cap only sheds
	// already-read messages (oldest first), never unread work.
	maxInboxMessages = 500
	// readRetention is how long a read message is kept for inbox/history views
	// before compaction may drop it.
	readRetention = 24 * time.Hour
)

// Message is one directed message in a recipient's inbox.
type Message struct {
	ID   string    `json:"id"`   // per-inbox id, assigned from a high-water mark (max+1) so compaction never recycles one
	From string    `json:"from"` // sender id, or "human"/"daemon"
	To   string    `json:"to"`
	Body string    `json:"body"`
	TS   time.Time `json:"ts"`
	Read bool      `json:"read"`
}

// Store persists messages in an embedded FileDB "messages" collection.
type Store struct {
	mu  sync.Mutex
	db  *filedb.DB
	col *engine.Collection
	dir string
}

// New creates dir (if needed) and returns a store backed by a FileDB "messages"
// collection rooted at dir, with a secondary index on the recipient field.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := filedb.Open(dir, filedb.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("messages")
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := col.EnsureIndex(toField); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, col: col, dir: dir}, nil
}

// msgKey is the collection record key for a message: "<to>:<id>". A recipient id
// never contains ":" (store.SafeID rejects it), so the composite is unambiguous.
func msgKey(to, id string) string { return to + ":" + id }

// toRecord marshals a Message to a record body. TS is stored as an RFC3339Nano
// string; the recipient key is the collection record key, not a body field.
func toRecord(m Message) map[string]any {
	return map[string]any{
		"id":    m.ID,
		"from":  m.From,
		toField: m.To,
		"body":  m.Body,
		"ts":    m.TS.Format(time.RFC3339Nano),
		"read":  m.Read,
	}
}

// toMessage reconstructs a Message from a record body.
func toMessage(d map[string]any) Message {
	m := Message{}
	m.ID, _ = d["id"].(string)
	m.From, _ = d["from"].(string)
	m.To, _ = d[toField].(string)
	m.Body, _ = d["body"].(string)
	m.Read, _ = d["read"].(bool)
	if s, ok := d["ts"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			m.TS = t
		}
	}
	return m
}

// idLess orders per-inbox ids numerically, i.e. arrival order.
func idLess(a, b string) bool {
	ai, _ := strconv.Atoi(a)
	bi, _ := strconv.Atoi(b)
	return ai < bi
}

// inbox returns to's messages in arrival order (ascending id). Caller holds mu.
// A recipient with no messages yields an empty, non-nil slice.
func (s *Store) inbox(to string) ([]Message, error) {
	ids, ok := s.col.IndexLookup(toField, to)
	if !ok || len(ids) == 0 {
		return []Message{}, nil
	}
	ms := make([]Message, 0, len(ids))
	for _, id := range ids {
		r, err := s.col.Get(id)
		if err != nil {
			// The index only maps to live ids and we hold mu, so a read miss is
			// unexpected; skip it rather than blanking the whole inbox.
			continue
		}
		ms = append(ms, toMessage(r.Data))
	}
	sort.Slice(ms, func(i, j int) bool { return idLess(ms[i].ID, ms[j].ID) })
	return ms, nil
}

// nextID returns the next per-inbox id as a high-water mark (max existing id +
// 1), not len+1: compaction can drop messages, so a length-based id would
// collide with (or recycle) an id still referenced by a MarkRead call or a
// client's cached view. Non-numeric ids are ignored when scanning.
func nextID(ms []Message) string {
	max := 0
	for _, m := range ms {
		if n, err := strconv.Atoi(m.ID); err == nil && n > max {
			max = n
		}
	}
	return strconv.Itoa(max + 1)
}

// compact bounds an inbox so a long-lived agent's per-inbox scan cost stays
// small. It never drops unread messages (undelivered work); it drops read
// messages older than readTTL, and if the result still exceeds maxN it sheds the
// oldest read messages until within the cap (unread are kept even past the cap).
// Input is assumed in arrival order (ascending TS); that order is preserved.
func compact(ms []Message, maxN int, readTTL time.Duration) []Message {
	cutoff := time.Now().Add(-readTTL)
	kept := make([]Message, 0, len(ms))
	for _, m := range ms {
		if m.Read && m.TS.Before(cutoff) {
			continue // aged-out read message
		}
		kept = append(kept, m)
	}
	if len(kept) <= maxN {
		return kept
	}
	// Still over the cap: drop oldest read messages first (kept is in arrival
	// order, so front-to-back is oldest-first); never drop unread work.
	over := len(kept) - maxN
	out := make([]Message, 0, len(kept))
	for _, m := range kept {
		if over > 0 && m.Read {
			over--
			continue
		}
		out = append(out, m)
	}
	return out
}

// Append stores m in m.To's inbox, assigning a per-inbox ID (high-water mark)
// and TS, then compacting the inbox to its retention bounds. The freshly
// appended message is unread, so compaction never drops it.
func (s *Store) Append(m Message) (Message, error) {
	if err := store.SafeID(m.To); err != nil {
		return Message{}, ErrBadRecipient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.inbox(m.To)
	if err != nil {
		return Message{}, err
	}
	m.ID = nextID(cur)
	m.TS = time.Now().UTC()
	m.Read = false
	if _, _, err := s.col.InsertWithKey(msgKey(m.To, m.ID), toRecord(m)); err != nil {
		return Message{}, err
	}
	// Compact over the pre-existing messages plus the new one, then delete the
	// records the compaction shed. The new (unread) message is always kept.
	full := append(cur, m)
	kept := compact(full, maxInboxMessages, readRetention)
	keptIDs := make(map[string]bool, len(kept))
	for _, k := range kept {
		keptIDs[k.ID] = true
	}
	for _, old := range full {
		if keptIDs[old.ID] {
			continue
		}
		if err := s.col.DeleteByKey(msgKey(m.To, old.ID)); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
			return Message{}, err
		}
	}
	return m, nil
}

// Messages returns to's inbox in arrival order (read-only). Always non-nil.
func (s *Store) Messages(to string) ([]Message, error) {
	if err := store.SafeID(to); err != nil {
		return nil, ErrBadRecipient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inbox(to)
}

// All returns every message across all recipients' inboxes (read-only, no
// mark-read), in unspecified order. Backs the daemon's global, read-only
// message-traffic view. It reads the collection directly, so stray files in the
// data dir are inherently ignored.
func (s *Store) All() ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(results))
	for _, r := range results {
		out = append(out, toMessage(r.Data))
	}
	return out, nil
}

// DeleteInbox removes a recipient's entire inbox. A recipient with no messages
// is a no-op (nil). Backs cleanup when an agent is hard-deleted; safe because
// nothing (pipelines included) reads another agent's inbox to make progress.
func (s *Store) DeleteInbox(to string) error {
	if err := store.SafeID(to); err != nil {
		return ErrBadRecipient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, ok := s.col.IndexLookup(toField, to)
	if !ok {
		return nil
	}
	for _, id := range ids {
		r, err := s.col.Get(id)
		if err != nil {
			continue
		}
		if err := s.col.DeleteByKey(r.Key); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
			return err
		}
	}
	return nil
}

// MarkRead flags the given message IDs read in to's inbox. Unknown IDs are
// ignored; an already-read message is left untouched.
func (s *Store) MarkRead(to string, ids []string) error {
	if err := store.SafeID(to); err != nil {
		return ErrBadRecipient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		key := msgKey(to, id)
		r, err := s.col.GetByKey(key)
		if errors.Is(err, engine.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		m := toMessage(r.Data)
		if m.Read {
			continue
		}
		m.Read = true
		if _, err := s.col.UpdateByKey(key, toRecord(m)); err != nil {
			return err
		}
	}
	return nil
}

// TakeFirstUnread atomically finds the oldest unread message in to's inbox
// matching from ("" = any sender), marks it read, and returns it. ok is false
// when nothing matches.
func (s *Store) TakeFirstUnread(to, from string) (Message, bool, error) {
	if err := store.SafeID(to); err != nil {
		return Message{}, false, ErrBadRecipient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, err := s.inbox(to)
	if err != nil {
		return Message{}, false, err
	}
	for _, m := range ms {
		if m.Read {
			continue
		}
		if from != "" && m.From != from {
			continue
		}
		m.Read = true
		if _, err := s.col.UpdateByKey(msgKey(to, m.ID), toRecord(m)); err != nil {
			return Message{}, false, err
		}
		return m, true, nil
	}
	return Message{}, false, nil
}

// Close flushes and releases the underlying FileDB collection (stopping its
// background compaction goroutine). The daemon calls it on shutdown.
func (s *Store) Close() error {
	return s.db.Close()
}
