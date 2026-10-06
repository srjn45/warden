// Package knownprompts persists prompt shapes the Fast-Brain has already read, so
// the next occurrence of the same menu is recognized without a model call.
//
// An entry holds only the SHAPE of a prompt — the question and the ordered option
// labels, with variable spans (quoted commands, the prompt's Action) replaced by
// a placeholder — plus which option is the least-privilege "yes" and which
// options are standing grants. It never holds the concrete command or path a
// prompt was asking about. Matching a pane returns the CONCRETE labels as they
// appear on screen, so everything downstream (fingerprints, the approvals inbox)
// stays faithful to the pane.
//
// Entries live in an embedded ScrivaDB collection and are mirrored in an
// in-memory index loaded at open and updated on every write: lookups never touch
// disk.
package knownprompts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/agentbackend"
)

const (
	// SourceFastBrain marks an entry learned from a Fast-Brain reading.
	SourceFastBrain = "fast-brain"
	// KindTrust mirrors agentbackend.ApprovalKindTrust; the only other Kind is "".
	KindTrust = agentbackend.ApprovalKindTrust
)

var (
	ErrNotFound = errors.New("known prompt not found")
	// ErrInvalid is returned for a reading that cannot become a safe template.
	ErrInvalid = errors.New("invalid known prompt")
)

// Entry is one learned prompt shape.
type Entry struct {
	ID       string   `json:"id"`
	Backend  string   `json:"backend"`
	Question string   `json:"question"` // template
	Options  []string `json:"options"`  // label templates, top-down
	// Affirmative is the 1-based least-privilege "yes"; 0 when none.
	Affirmative int `json:"affirmative"`
	// Sticky[i] is true when option i+1 is a standing grant.
	Sticky     []bool    `json:"sticky"`
	Kind       string    `json:"kind,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Hits       int       `json:"hits"`
	CLIVersion string    `json:"cli_version,omitempty"`
	Source     string    `json:"source"`
}

// Reading is a concrete reading of one prompt, as the model produced it. Action
// is used only to template and is never stored.
type Reading struct {
	Question    string
	Action      string
	Options     []string
	Affirmative int
	Sticky      []bool // per option; nil means none
	Kind        string
}

// Match is a known prompt found in a live pane.
type Match struct {
	Entry *Entry
	// Options are the labels as they appear on screen.
	Options  []string
	Location agentbackend.MenuLocation
}

type indexed struct {
	e       *Entry
	res     []*regexp.Regexp
	literal int
}

// Store is the persistent set of known prompts.
type Store struct {
	mu      sync.RWMutex
	db      *scriva.DB
	col     *engine.Collection
	byID    map[string]*Entry
	backend map[string][]*indexed
	now     func() time.Time

	maxEntries int           // 0 = unbounded
	pruneAfter time.Duration // 0 = never prune by age
}

const (
	// DefaultMaxEntries bounds the store; the least recently seen entry is
	// evicted to stay under it.
	DefaultMaxEntries = 500
	// DefaultPruneAfter drops entries not seen for this long.
	DefaultPruneAfter = 90 * 24 * time.Hour
)

// New opens (creating if needed) the store at <dir>/known-prompts-db and loads
// the in-memory index.
func New(dir string) (*Store, error) {
	dbDir := filepath.Join(dir, "known-prompts-db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("known_prompts")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, col: col, byID: map[string]*Entry{}, backend: map[string][]*indexed{}, now: time.Now, maxEntries: DefaultMaxEntries, pruneAfter: DefaultPruneAfter}
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, row := range rows {
		var e Entry
		if err := decode(row.Data, &e); err != nil {
			continue // a corrupt row is skipped, not fatal
		}
		_ = s.index(&e)
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func encode(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func decode(m map[string]any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// entryID keys an entry by backend plus normalized template, so one shape is
// one record.
func entryID(backend, question string, options []string) string {
	parts := []string{backend, normalize(question)}
	for _, o := range options {
		parts = append(parts, normalize(o))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:])[:24]
}

// buildEntry templates a reading into an unsaved entry.
func buildEntry(backend string, r Reading) (*Entry, error) {
	if strings.TrimSpace(backend) == "" {
		return nil, fmt.Errorf("%w: no backend", ErrInvalid)
	}
	if len(r.Options) < 2 {
		return nil, fmt.Errorf("%w: need at least two options", ErrInvalid)
	}
	if r.Kind != "" && r.Kind != KindTrust {
		return nil, fmt.Errorf("%w: kind %q", ErrInvalid, r.Kind)
	}
	if r.Affirmative < 0 || r.Affirmative > len(r.Options) {
		return nil, fmt.Errorf("%w: affirmative %d out of range", ErrInvalid, r.Affirmative)
	}
	if r.Sticky != nil && len(r.Sticky) != len(r.Options) {
		return nil, fmt.Errorf("%w: sticky flags do not match options", ErrInvalid)
	}
	e := &Entry{
		Backend:     backend,
		Question:    Template(r.Question, r.Action),
		Affirmative: r.Affirmative,
		Sticky:      make([]bool, len(r.Options)),
		Kind:        r.Kind,
		Source:      SourceFastBrain,
	}
	copy(e.Sticky, r.Sticky)
	for _, o := range r.Options {
		t := Template(o, r.Action)
		// A label with no literal text would match any line.
		if literalLen(t) == 0 {
			return nil, fmt.Errorf("%w: option %q has no literal text", ErrInvalid, o)
		}
		e.Options = append(e.Options, t)
	}
	e.ID = entryID(backend, e.Question, e.Options)
	return e, nil
}

// index adds e to the in-memory index, replacing an entry with the same ID.
// Callers hold s.mu for writing (or are single-threaded in New).
func (s *Store) index(e *Entry) error {
	ix := &indexed{e: e}
	for _, o := range e.Options {
		re, err := compile(o)
		if err != nil {
			return err
		}
		ix.res = append(ix.res, re)
		ix.literal += literalLen(o)
	}
	if old := s.byID[e.ID]; old != nil {
		list := s.backend[old.Backend]
		for i, x := range list {
			if x.e.ID == e.ID {
				s.backend[old.Backend] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
	}
	s.byID[e.ID] = e
	s.backend[e.Backend] = append(s.backend[e.Backend], ix)
	return nil
}

// Learn records a reading's shape. A shape already known only has its
// last_seen_at (and cli version, when given) refreshed; created reports whether
// a new entry was written.
func (s *Store) Learn(ctx context.Context, backend, cliVersion string, r Reading) (e Entry, created bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	n, err := buildEntry(backend, r)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if cur := s.byID[n.ID]; cur != nil {
		upd := *cur
		upd.LastSeenAt = now
		if cliVersion != "" {
			upd.CLIVersion = cliVersion
		}
		return upd, false, s.save(&upd, true)
	}
	n.CreatedAt, n.LastSeenAt, n.CLIVersion = now, now, cliVersion
	if err = s.save(n, false); err != nil {
		return
	}
	s.enforceLocked(now, n.ID)
	return *n, true, nil
}

// SetLimits bounds the store: at most maxEntries entries (the least recently
// seen is evicted first) and none unseen for longer than pruneAfter. Zero
// disables the respective bound. Limits apply from the next Learn or Prune.
func (s *Store) SetLimits(maxEntries int, pruneAfter time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxEntries, s.pruneAfter = maxEntries, pruneAfter
}

// Prune applies the store's limits now and reports how many entries it removed.
func (s *Store) Prune() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enforceLocked(s.now(), "")
}

// enforceLocked drops entries unseen for pruneAfter, then evicts the least
// recently seen until the store fits maxEntries. keep (the entry just learned)
// is never evicted. A failed delete leaves the entry for the next pass.
func (s *Store) enforceLocked(now time.Time, keep string) int {
	removed := 0
	drop := func(e *Entry) {
		if s.deleteLocked(e.ID) == nil {
			removed++
		}
	}
	if s.pruneAfter > 0 {
		for _, e := range s.byID {
			if e.ID != keep && now.Sub(e.LastSeenAt) > s.pruneAfter {
				drop(e)
			}
		}
	}
	if s.maxEntries > 0 && len(s.byID) > s.maxEntries {
		all := make([]*Entry, 0, len(s.byID))
		for _, e := range s.byID {
			if e.ID != keep {
				all = append(all, e)
			}
		}
		sort.Slice(all, func(i, j int) bool {
			if !all[i].LastSeenAt.Equal(all[j].LastSeenAt) {
				return all[i].LastSeenAt.Before(all[j].LastSeenAt)
			}
			return all[i].ID < all[j].ID
		})
		for _, e := range all {
			if len(s.byID) <= s.maxEntries {
				break
			}
			drop(e)
		}
	}
	return removed
}

// save persists e then refreshes the index (memory changes only on success).
func (s *Store) save(e *Entry, update bool) error {
	rec, err := encode(e)
	if err != nil {
		return err
	}
	if update {
		_, err = s.col.UpdateByKey(e.ID, rec)
	} else {
		_, _, err = s.col.InsertWithKey(e.ID, rec)
	}
	if err != nil {
		return err
	}
	cp := *e
	return s.index(&cp)
}

// Hit records that an entry just recognized a live prompt.
func (s *Store) Hit(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.byID[id]
	if cur == nil {
		return ErrNotFound
	}
	upd := *cur
	upd.Hits++
	upd.LastSeenAt = s.now()
	return s.save(&upd, true)
}

// Delete forgets an entry.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(id)
}

// DeleteAll forgets every entry and returns how many were removed.
func (s *Store) DeleteAll(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.byID))
	for id := range s.byID {
		ids = append(ids, id)
	}
	n := 0
	for _, id := range ids {
		if err := s.deleteLocked(id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) deleteLocked(id string) error {
	cur := s.byID[id]
	if cur == nil {
		return ErrNotFound
	}
	if err := s.col.DeleteByKey(id); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
		return err
	}
	list := s.backend[cur.Backend]
	for i, x := range list {
		if x.e.ID == id {
			s.backend[cur.Backend] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	delete(s.byID, id)
	return nil
}

// List returns every entry, oldest first.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.byID))
	for _, e := range s.byID {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Match finds a known prompt of backend showing at the bottom of pane. Only
// entries of that backend are considered. When several match, the one with the
// most literal label text wins (the most specific shape), then the most hit. It
// reads memory only.
func (s *Store) Match(backend, pane string) (*Match, bool) {
	s.mu.RLock()
	cands := append([]*indexed(nil), s.backend[backend]...)
	s.mu.RUnlock()

	var best *Match
	var bestIx *indexed
	for _, ix := range cands {
		loc, labels, ok := agentbackend.LocateMatching(pane, len(ix.res), func(i int, label string) bool {
			return ix.res[i].MatchString(label)
		})
		if !ok {
			continue
		}
		if bestIx == nil || ix.literal > bestIx.literal ||
			(ix.literal == bestIx.literal && ix.e.Hits > bestIx.e.Hits) {
			cp := *ix.e
			best, bestIx = &Match{Entry: &cp, Options: labels, Location: loc}, ix
		}
	}
	return best, best != nil
}
