// Package groupstore persists collaboration groups (docs/specs/
// 2026-08-26-collaboration-groups.md §4.3): one record per group, each holding
// only the lean roster of its members. It is deliberately a small, mostly-static
// store — transcripts and message logs never go in a group record (those stay in
// the inbox store), which is what previously tripped the oversized-record
// >64 KB ReadAt / index-corruption failure modes.
//
// It mirrors internal/schedule/store.go and internal/backendstore/store.go: an
// embedded ScrivaDB collection opened SyncModeNone, one record per group keyed by
// its name. This is a fresh store — there is no legacy JSON to import, so none of
// the schedule store's sentinel/import machinery applies.
package groupstore

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

var (
	// ErrNotFound is the store-boundary translation of engine.ErrKeyNotFound.
	ErrNotFound = errors.New("group not found")
	// ErrExists is the store-boundary translation of engine.ErrDuplicateKey.
	ErrExists = errors.New("group already exists")
)

// Member is one seat in a collaboration group. It carries only the lean roster
// descriptor the design (§4.1/§4.2) allows — an agent id, its project key (the
// normalized git-remote key from B2), a one-line project summary, and when it
// joined. Never anything transcript- or log-sized.
type Member struct {
	AgentID    string    `json:"agent_id"`
	ProjectKey string    `json:"project_key"`
	Summary    string    `json:"summary,omitempty"`
	JoinedAt   time.Time `json:"joined_at"`
}

// Group is one durable collaboration group, keyed by Name in ScrivaDB. The group
// is durable (survives daemon restarts); its membership is live (tied to running
// agents) and re-seated on recover. The record stays small and mostly static.
type Group struct {
	Name    string   `json:"name"`
	Members []Member `json:"members"`
}

// Store persists groups as records in an embedded ScrivaDB "groups" collection,
// one record per group keyed by its Name. Opened SyncModeNone: this is a
// localhost daemon store, so last-write-survives-power-loss is not required
// (append-only segments rule out torn reads regardless).
//
// The daemon is the only holder. A single mutex serialises the compound
// read-modify-write methods (Create's exists-check + insert, Update's
// read-modify-write), mirroring the schedule/backend stores; ScrivaDB does its
// own per-collection locking, so the store mutex only guards the read-then-write
// critical sections. Read-only methods take it too for a behaviour-identical
// mutex model.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// NewStore opens (creating if needed) the ScrivaDB-backed group store at dir
// (~/.warden/groups). This is a fresh store: there is no legacy JSON to import.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("groups")
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// toRecord decomposes a Group into a ScrivaDB record body via a JSON round-trip,
// so its fields stay real in the store rather than an opaque blob. The engine
// stamps the reserved key field on InsertWithKey/UpdateByKey, so it must NOT be
// present here.
func toRecord(g *Group) (map[string]any, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromRecord reconstructs a Group from a record body. The reserved key field the
// engine stamped into the map is harmlessly dropped on unmarshal (Group has no
// matching json tag).
func fromRecord(d map[string]any) (*Group, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var g Group
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// Create persists a new group, returning ErrExists if its name is taken.
func (s *Store) Create(g *Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := toRecord(g)
	if err != nil {
		return err
	}
	if _, _, err := s.col.InsertWithKey(g.Name, rec); err != nil {
		if errors.Is(err, engine.ErrDuplicateKey) {
			return ErrExists
		}
		return err
	}
	return nil
}

// Get returns one group by name, or ErrNotFound.
func (s *Store) Get(name string) (*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(name)
}

// get reads the group keyed name, mapping a key miss to ErrNotFound. The caller
// holds mu.
func (s *Store) get(name string) (*Group, error) {
	r, err := s.col.GetByKey(name)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

// List returns all groups sorted by name. An undecodable record is skipped rather
// than failing the whole scan.
func (s *Store) List() ([]*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Group, 0, len(results))
	for _, r := range results {
		g, err := fromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GroupsForAgent returns all groups that contain agentID as a member, sorted
// by name. An empty (non-nil) slice is returned when the agent holds no seats.
// Undecodable records are skipped rather than failing the scan.
func (s *Store) GroupsForAgent(agentID string) ([]*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Group, 0)
	for _, r := range results {
		g, err := fromRecord(r.Data)
		if err != nil {
			continue
		}
		for _, m := range g.Members {
			if m.AgentID == agentID {
				out = append(out, g)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Update applies fn to the stored group under the lock and writes it back
// atomically. It returns ErrNotFound if the name is absent. This is the seam
// join/leave (B3/B6) use to add or remove a seat without racing.
func (s *Store) Update(name string, fn func(*Group)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.get(name)
	if err != nil {
		return err
	}
	fn(g)
	rec, err := toRecord(g)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(name, rec)
	return err
}

// Delete removes a group by name, returning ErrNotFound if absent.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(name)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// Close flushes the ScrivaDB index and stops its background compaction goroutine.
func (s *Store) Close() error {
	return s.db.Close()
}
