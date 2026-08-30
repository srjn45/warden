package projectstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

var (
	// ErrGroupNotFound is the store-boundary translation of engine.ErrKeyNotFound
	// for the project_groups collection.
	ErrGroupNotFound = errors.New("project group not found")
	// ErrGroupExists is returned when creating a group whose id already exists.
	ErrGroupExists = errors.New("project group already exists")
	// ErrInvalidGroupName is returned when a group is created/updated with a blank
	// name. Unlike a project id, a group's id is minted for it, so the name is the
	// user-facing required field.
	ErrInvalidGroupName = errors.New("project group name cannot be empty")
)

// newGroupID mints a random RFC-4122 v4 UUID string used as a group's stable key
// when the caller supplies none. It mirrors store.NewSessionID but stays local to
// avoid a package dependency; a random-source failure is surfaced to the caller.
func newGroupID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

// groupFromRecord reconstructs a ProjectGroup from a record body via a JSON
// round-trip; the reserved key field the engine stamped in is dropped on unmarshal.
func groupFromRecord(d map[string]any) (ProjectGroup, error) {
	var out ProjectGroup
	if err := jsonRoundTrip(d, &out); err != nil {
		return ProjectGroup{}, err
	}
	return out, nil
}

// dedupeIDs returns ids with blanks removed and duplicates collapsed, preserving
// first-seen order — the canonical form for a group's ProjectIDs.
func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// CreateGroup inserts a new project group. A blank g.ID is filled with a minted
// UUID; a supplied id that already exists yields ErrGroupExists. The name is
// required. ProjectIDs are de-duplicated. Timestamps are stamped. Returns the
// stored group (so the caller learns the minted id).
func (s *Store) CreateGroup(g ProjectGroup) (ProjectGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.Name == "" {
		return ProjectGroup{}, ErrInvalidGroupName
	}
	if g.ID == "" {
		id, err := newGroupID()
		if err != nil {
			return ProjectGroup{}, err
		}
		g.ID = id
	}
	now := s.now()
	g.CreatedAt = now
	g.UpdatedAt = now
	g.ProjectIDs = dedupeIDs(g.ProjectIDs)
	rec, err := toRecord(g)
	if err != nil {
		return ProjectGroup{}, err
	}
	if _, _, err := s.groups.InsertWithKey(g.ID, rec); err != nil {
		if errors.Is(err, engine.ErrDuplicateKey) {
			return ProjectGroup{}, ErrGroupExists
		}
		return ProjectGroup{}, err
	}
	return g, nil
}

// GetGroup returns one group by id, or ErrGroupNotFound.
func (s *Store) GetGroup(id string) (ProjectGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getGroup(id)
}

// getGroup reads the group keyed id, mapping a key miss to ErrGroupNotFound. The
// caller holds s.mu.
func (s *Store) getGroup(id string) (ProjectGroup, error) {
	r, err := s.groups.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ProjectGroup{}, ErrGroupNotFound
	}
	if err != nil {
		return ProjectGroup{}, err
	}
	return groupFromRecord(r.Data)
}

// ListGroups returns every group sorted by name (then id for a stable tie-break).
// An undecodable record is skipped rather than failing the scan.
func (s *Store) ListGroups() ([]ProjectGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.groups.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectGroup, 0, len(results))
	for _, r := range results {
		g, err := groupFromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// UpdateGroup overwrites an existing group's name and membership in place, keyed by
// g.ID. CreatedAt is preserved from the stored row; UpdatedAt is refreshed. The name
// is required and ProjectIDs are de-duplicated. Returns ErrGroupNotFound if the id
// is absent, else the updated group.
func (s *Store) UpdateGroup(g ProjectGroup) (ProjectGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.ID == "" {
		return ProjectGroup{}, ErrGroupNotFound
	}
	if g.Name == "" {
		return ProjectGroup{}, ErrInvalidGroupName
	}
	prev, err := s.getGroup(g.ID)
	if err != nil {
		return ProjectGroup{}, err
	}
	g.CreatedAt = prev.CreatedAt
	g.UpdatedAt = s.now()
	g.ProjectIDs = dedupeIDs(g.ProjectIDs)
	rec, err := toRecord(g)
	if err != nil {
		return ProjectGroup{}, err
	}
	if _, err := s.groups.UpdateByKey(g.ID, rec); err != nil {
		return ProjectGroup{}, err
	}
	return g, nil
}

// DeleteGroup hard-removes the group row keyed id. A missing row is not an error
// (idempotent). The member projects themselves are untouched.
func (s *Store) DeleteGroup(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.groups.DeleteByKey(id); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
		return err
	}
	return nil
}

// AddProjectToGroup appends projectID to the group's membership (RMW), de-duplicated
// so a repeat add is a no-op. Returns ErrGroupNotFound if the group is absent, else
// the updated group.
func (s *Store) AddProjectToGroup(groupID, projectID string) (ProjectGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID == "" {
		return ProjectGroup{}, ErrInvalidID
	}
	g, err := s.getGroup(groupID)
	if err != nil {
		return ProjectGroup{}, err
	}
	g.ProjectIDs = dedupeIDs(append(g.ProjectIDs, projectID))
	g.UpdatedAt = s.now()
	rec, err := toRecord(g)
	if err != nil {
		return ProjectGroup{}, err
	}
	if _, err := s.groups.UpdateByKey(g.ID, rec); err != nil {
		return ProjectGroup{}, err
	}
	return g, nil
}

// RemoveProjectFromGroup drops projectID from the group's membership (RMW).
// Removing an absent member is a no-op (not an error). Returns ErrGroupNotFound if
// the group is absent, else the updated group.
func (s *Store) RemoveProjectFromGroup(groupID, projectID string) (ProjectGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.getGroup(groupID)
	if err != nil {
		return ProjectGroup{}, err
	}
	filtered := g.ProjectIDs[:0:0]
	for _, id := range g.ProjectIDs {
		if id != projectID {
			filtered = append(filtered, id)
		}
	}
	g.ProjectIDs = filtered
	g.UpdatedAt = s.now()
	rec, err := toRecord(g)
	if err != nil {
		return ProjectGroup{}, err
	}
	if _, err := s.groups.UpdateByKey(g.ID, rec); err != nil {
		return ProjectGroup{}, err
	}
	return g, nil
}
