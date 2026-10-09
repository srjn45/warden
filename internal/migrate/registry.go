package migrate

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	ErrDuplicateMigration = errors.New("migrate: duplicate migration ID")
	ErrMigrationNotFound  = errors.New("migrate: migration not found")
	ErrInvalidPath        = errors.New("migrate: no migration path between versions")
	ErrDowngrade          = errors.New("migrate: downgrades are not supported (use rollback)")
)

// DefaultRegistry is the process-global migration registry.
var DefaultRegistry = NewRegistry()

// Register adds a migration to DefaultRegistry.
func Register(m Migration) error {
	return DefaultRegistry.Register(m)
}

// Plan computes the migration chain in DefaultRegistry.
func Plan(from, to int) ([]Migration, error) {
	return DefaultRegistry.Plan(from, to)
}

// Registry stores migrations indexed by ID and ordered by version.
type Registry struct {
	mu         sync.RWMutex
	migrations map[string]Migration
	list       []Migration
}

// NewRegistry initializes an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		migrations: make(map[string]Migration),
	}
}

// Register adds a migration to the registry.
func (r *Registry) Register(m Migration) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if m.ID == "" {
		return errors.New("migrate: migration ID cannot be empty")
	}
	if _, exists := r.migrations[m.ID]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateMigration, m.ID)
	}
	if m.From >= m.To {
		return fmt.Errorf("migrate: invalid migration %s: From (%d) must be < To (%d)", m.ID, m.From, m.To)
	}

	r.migrations[m.ID] = m
	r.list = append(r.list, m)
	sort.Slice(r.list, func(i, j int) bool {
		if r.list[i].From != r.list[j].From {
			return r.list[i].From < r.list[j].From
		}
		return r.list[i].To < r.list[j].To
	})
	return nil
}

// Get looks up a migration by ID.
func (r *Registry) Get(id string) (Migration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.migrations[id]
	return m, ok
}

// All returns a slice of all registered migrations sorted by From version.
func (r *Registry) All() []Migration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Migration, len(r.list))
	copy(out, r.list)
	return out
}

// Plan finds the ordered sequence of migrations required to go from schema version
// `from` to `to`.
func (r *Registry) Plan(from, to int) ([]Migration, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if from == to {
		return nil, nil
	}
	if from > to {
		return nil, fmt.Errorf("%w: cannot migrate from schema %d to %d", ErrDowngrade, from, to)
	}

	var plan []Migration
	curr := from
	for curr < to {
		found := false
		for _, m := range r.list {
			if m.From == curr && m.To <= to {
				plan = append(plan, m)
				curr = m.To
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: cannot reach schema %d from schema %d (stopped at %d)", ErrInvalidPath, to, from, curr)
		}
	}
	return plan, nil
}
