package agentstore

import (
	"sort"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// Snapshot is an immutable, versioned in-memory image of active and closed agent collections.
type Snapshot struct {
	Version       uint64
	BuiltAt       time.Time
	Agents        []*Agent          // sorted newest UpdatedAt first
	ByID          map[string]*Agent // keyed by Agent.ID
	ByName        map[string]*Agent // keyed by Agent.Name
	Closed        []*Agent          // sorted newest UpdatedAt first
	ClosedByID    map[string]*Agent // keyed by Agent.ID
	ClosedSkipped int
	Verified      bool
}

// Get looks up an active agent by ID in O(1) time without scanning.
func (snap *Snapshot) Get(id string) (*Agent, error) {
	if snap == nil {
		return nil, ErrNotFound
	}
	a, ok := snap.ByID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneAgent(a), nil
}

// GetByNameOrID looks up by name first, then fallback to ID in O(1) time without scanning.
func (snap *Snapshot) GetByNameOrID(nameOrID string) (*Agent, error) {
	if snap == nil || nameOrID == "" {
		return nil, ErrNotFound
	}
	if a, ok := snap.ByName[nameOrID]; ok {
		return cloneAgent(a), nil
	}
	if a, ok := snap.ByID[nameOrID]; ok {
		return cloneAgent(a), nil
	}
	return nil, ErrNotFound
}

// List returns all active agents, newest UpdatedAt first.
func (snap *Snapshot) List() []*Agent {
	if snap == nil {
		return nil
	}
	out := make([]*Agent, len(snap.Agents))
	for i, a := range snap.Agents {
		out[i] = cloneAgent(a)
	}
	return out
}

// ListClosed returns all closed agents, newest UpdatedAt first.
func (snap *Snapshot) ListClosed() []*Agent {
	if snap == nil {
		return nil
	}
	out := make([]*Agent, len(snap.Closed))
	for i, a := range snap.Closed {
		out[i] = cloneAgent(a)
	}
	return out
}

// ListClosedDegraded returns all closed agents and the count of skipped undecodable records.
func (snap *Snapshot) ListClosedDegraded() ([]*Agent, int) {
	if snap == nil {
		return nil, 0
	}
	return snap.ListClosed(), snap.ClosedSkipped
}

func cloneAgent(a *Agent) *Agent {
	if a == nil {
		return nil
	}
	out := *a
	if a.Tags != nil {
		out.Tags = make([]string, len(a.Tags))
		copy(out.Tags, a.Tags)
	}
	if a.ExitCode != nil {
		code := *a.ExitCode
		out.ExitCode = &code
	}
	if a.LastRestartAt != nil {
		t := *a.LastRestartAt
		out.LastRestartAt = &t
	}
	if a.ForceCompact != nil {
		fc := *a.ForceCompact
		out.ForceCompact = &fc
	}
	if a.LastCompactAt != nil {
		t := *a.LastCompactAt
		out.LastCompactAt = &t
	}
	if a.RateLimitedAt != nil {
		t := *a.RateLimitedAt
		out.RateLimitedAt = &t
	}
	if a.RateLimitRestoreAt != nil {
		t := *a.RateLimitRestoreAt
		out.RateLimitRestoreAt = &t
	}
	if a.ChildAgents != nil {
		out.ChildAgents = make([]string, len(a.ChildAgents))
		copy(out.ChildAgents, a.ChildAgents)
	}
	if a.ChildPipelines != nil {
		out.ChildPipelines = make([]string, len(a.ChildPipelines))
		copy(out.ChildPipelines, a.ChildPipelines)
	}
	if a.ChildAutopilots != nil {
		out.ChildAutopilots = make([]string, len(a.ChildAutopilots))
		copy(out.ChildAutopilots, a.ChildAutopilots)
	}
	if a.QuotaBinding != nil {
		qb := *a.QuotaBinding
		out.QuotaBinding = &qb
	}
	if a.BackendRecovery != nil {
		br := *a.BackendRecovery
		out.BackendRecovery = &br
	}
	if a.Events != nil {
		out.Events = append([]store.Event(nil), a.Events...)
	}
	return &out
}

func buildSnapshot(version uint64, builtAt time.Time, activeRows []*Agent, closedRows []*Agent, closedSkipped int) *Snapshot {
	snap := &Snapshot{
		Version:       version,
		BuiltAt:       builtAt,
		Agents:        make([]*Agent, 0, len(activeRows)),
		ByID:          make(map[string]*Agent, len(activeRows)),
		ByName:        make(map[string]*Agent, len(activeRows)),
		Closed:        make([]*Agent, 0, len(closedRows)),
		ClosedByID:    make(map[string]*Agent, len(closedRows)),
		ClosedSkipped: closedSkipped,
		Verified:      true,
	}
	for _, a := range activeRows {
		ac := cloneAgent(a)
		snap.Agents = append(snap.Agents, ac)
		snap.ByID[ac.ID] = ac
		if ac.Name != "" {
			snap.ByName[ac.Name] = ac
		}
	}
	sort.Slice(snap.Agents, func(i, j int) bool {
		return snap.Agents[i].UpdatedAt.After(snap.Agents[j].UpdatedAt)
	})

	for _, c := range closedRows {
		cc := cloneAgent(c)
		snap.Closed = append(snap.Closed, cc)
		snap.ClosedByID[cc.ID] = cc
	}
	sort.Slice(snap.Closed, func(i, j int) bool {
		return snap.Closed[i].UpdatedAt.After(snap.Closed[j].UpdatedAt)
	})
	return snap
}

func (s *Store) publishInsert(cur *Snapshot, a *Agent) {
	newSnap := &Snapshot{
		Version:    1,
		BuiltAt:    time.Now().UTC(),
		ByID:       make(map[string]*Agent),
		ByName:     make(map[string]*Agent),
		ClosedByID: make(map[string]*Agent),
		Verified:   true,
	}
	if cur != nil {
		newSnap.Version = cur.Version + 1
		newSnap.ClosedSkipped = cur.ClosedSkipped
		newSnap.Closed = cur.Closed
		for k, v := range cur.ClosedByID {
			newSnap.ClosedByID[k] = v
		}
		for k, v := range cur.ByID {
			newSnap.ByID[k] = v
		}
		for k, v := range cur.ByName {
			newSnap.ByName[k] = v
		}
	}
	savedAgent := cloneAgent(a)
	newSnap.ByID[savedAgent.ID] = savedAgent
	if savedAgent.Name != "" {
		newSnap.ByName[savedAgent.Name] = savedAgent
	}
	newSnap.Agents = make([]*Agent, 0, len(newSnap.ByID))
	for _, ag := range newSnap.ByID {
		newSnap.Agents = append(newSnap.Agents, ag)
	}
	sort.Slice(newSnap.Agents, func(i, j int) bool {
		return newSnap.Agents[i].UpdatedAt.After(newSnap.Agents[j].UpdatedAt)
	})
	s.snap.Store(newSnap)
}

func (s *Store) publishUpdate(cur *Snapshot, a *Agent) {
	newSnap := &Snapshot{
		Version:    1,
		BuiltAt:    time.Now().UTC(),
		ByID:       make(map[string]*Agent),
		ByName:     make(map[string]*Agent),
		ClosedByID: make(map[string]*Agent),
		Verified:   true,
	}
	var oldName string
	if cur != nil {
		newSnap.Version = cur.Version + 1
		newSnap.ClosedSkipped = cur.ClosedSkipped
		newSnap.Closed = cur.Closed
		for k, v := range cur.ClosedByID {
			newSnap.ClosedByID[k] = v
		}
		for k, v := range cur.ByID {
			if k == a.ID {
				oldName = v.Name
			} else {
				newSnap.ByID[k] = v
			}
		}
		for k, v := range cur.ByName {
			newSnap.ByName[k] = v
		}
	}
	if oldName != "" && oldName != a.Name {
		delete(newSnap.ByName, oldName)
	}
	savedAgent := cloneAgent(a)
	newSnap.ByID[savedAgent.ID] = savedAgent
	if savedAgent.Name != "" {
		newSnap.ByName[savedAgent.Name] = savedAgent
	}
	newSnap.Agents = make([]*Agent, 0, len(newSnap.ByID))
	for _, ag := range newSnap.ByID {
		newSnap.Agents = append(newSnap.Agents, ag)
	}
	sort.Slice(newSnap.Agents, func(i, j int) bool {
		return newSnap.Agents[i].UpdatedAt.After(newSnap.Agents[j].UpdatedAt)
	})
	s.snap.Store(newSnap)
}

func (s *Store) publishDelete(cur *Snapshot, id string) {
	newSnap := &Snapshot{
		Version:    1,
		BuiltAt:    time.Now().UTC(),
		ByID:       make(map[string]*Agent),
		ByName:     make(map[string]*Agent),
		ClosedByID: make(map[string]*Agent),
		Verified:   true,
	}
	var oldName string
	if cur != nil {
		newSnap.Version = cur.Version + 1
		newSnap.ClosedSkipped = cur.ClosedSkipped
		newSnap.Closed = cur.Closed
		for k, v := range cur.ClosedByID {
			newSnap.ClosedByID[k] = v
		}
		for k, v := range cur.ByID {
			if k == id {
				oldName = v.Name
			} else {
				newSnap.ByID[k] = v
			}
		}
		for k, v := range cur.ByName {
			if k != oldName {
				newSnap.ByName[k] = v
			}
		}
	}
	newSnap.Agents = make([]*Agent, 0, len(newSnap.ByID))
	for _, ag := range newSnap.ByID {
		newSnap.Agents = append(newSnap.Agents, ag)
	}
	sort.Slice(newSnap.Agents, func(i, j int) bool {
		return newSnap.Agents[i].UpdatedAt.After(newSnap.Agents[j].UpdatedAt)
	})
	s.snap.Store(newSnap)
}

func (s *Store) publishArchive(cur *Snapshot, id string, archived *Agent) {
	newSnap := &Snapshot{
		Version:    1,
		BuiltAt:    time.Now().UTC(),
		ByID:       make(map[string]*Agent),
		ByName:     make(map[string]*Agent),
		ClosedByID: make(map[string]*Agent),
		Verified:   true,
	}
	var oldName string
	if cur != nil {
		newSnap.Version = cur.Version + 1
		newSnap.ClosedSkipped = cur.ClosedSkipped
		for k, v := range cur.ClosedByID {
			newSnap.ClosedByID[k] = v
		}
		for k, v := range cur.ByID {
			if k == id {
				oldName = v.Name
			} else {
				newSnap.ByID[k] = v
			}
		}
		for k, v := range cur.ByName {
			if k != oldName {
				newSnap.ByName[k] = v
			}
		}
	}
	savedArchived := cloneAgent(archived)
	newSnap.ClosedByID[id] = savedArchived

	newSnap.Agents = make([]*Agent, 0, len(newSnap.ByID))
	for _, ag := range newSnap.ByID {
		newSnap.Agents = append(newSnap.Agents, ag)
	}
	sort.Slice(newSnap.Agents, func(i, j int) bool {
		return newSnap.Agents[i].UpdatedAt.After(newSnap.Agents[j].UpdatedAt)
	})

	newSnap.Closed = make([]*Agent, 0, len(newSnap.ClosedByID))
	for _, ag := range newSnap.ClosedByID {
		newSnap.Closed = append(newSnap.Closed, ag)
	}
	sort.Slice(newSnap.Closed, func(i, j int) bool {
		return newSnap.Closed[i].UpdatedAt.After(newSnap.Closed[j].UpdatedAt)
	})

	s.snap.Store(newSnap)
}
