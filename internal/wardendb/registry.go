package wardendb

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
)

// Registry errors. Each is wrapped with the offending collection name.
var (
	// ErrUnknownCollection: the name is not in the contract catalogue.
	ErrUnknownCollection = errors.New("wardendb: unknown collection")
	// ErrDuplicateCollection: the collection is already registered.
	ErrDuplicateCollection = errors.New("wardendb: collection already registered")
	// ErrNotRegistered: the collection was never registered on this handle.
	ErrNotRegistered = errors.New("wardendb: collection not registered")
	// ErrInvalidSpec: the declaration itself is malformed.
	ErrInvalidSpec = errors.New("wardendb: invalid collection spec")
	// ErrTypeMismatch: the collection is registered with another record type.
	ErrTypeMismatch = errors.New("wardendb: collection record type mismatch")
)

// catalogue is the collection list of the collection contract §2, in contract
// order. A collection that is not listed here cannot be registered; new
// schema-2 collections are added here together with their contract.
var catalogue = []string{
	"agents",
	"terminals",
	"projects",
	"project_groups",
	"plans",
	"plan_exports",
	"pipelines",
	"pipeline_jobs",
	"autopilot_runs",
	"context_entries",
	"messages",
	"backends",
	"backend_models",
	"backend_roles",
	"backend_settings",
	"usage_snapshots",
	"quota_impact_fences",
	"snapshots",
	"savings_events",
	"savings_calibration",
	"spend_records",
	"schedules",
	"known_prompts",
	"plan_sync_envelopes",
	"metrics_samples",
	"id_sequences",
	// Added by the operation-journal contract §1.2.
	"operations",
}

// Catalogue returns the names of every collection that may be registered.
func Catalogue() []string { return append([]string(nil), catalogue...) }

func inCatalogue(name string) bool {
	for _, n := range catalogue {
		if n == name {
			return true
		}
	}
	return false
}

// Spec declares one collection.
type Spec struct {
	// Name must be a catalogue name.
	Name string
	// Version is the collection's record-schema version (>= 1), recorded in
	// the registry for the owning package and for diagnostics.
	Version int
	// Options are the collection's engine options (unique indexes, record and
	// byte limits). Durability and compaction cadence belong to the strict
	// policy: an option that sets them is overridden.
	Options []scriva.CollectionOption
}

// CollectionInfo describes one registered collection.
type CollectionInfo struct {
	Name    string
	Version int
	// RecordType is the Go type the collection was registered with.
	RecordType string
}

type entry struct {
	spec   Spec
	typ    reflect.Type
	col    *engine.Collection
	closed bool
}

// Collection is the typed accessor for one registered collection. T is the
// owning package's record type; it binds the handle to that type so a second
// package cannot reach the collection under a different shape.
type Collection[T any] struct {
	db *DB
	e  *entry
}

// Register declares a collection on db and returns its typed accessor. The
// name must be in the catalogue (ErrUnknownCollection), the spec must be well
// formed (ErrInvalidSpec) and the collection must not already be registered
// (ErrDuplicateCollection). Registration order defines close order.
//
// The collection is opened under the strict policy and verified against its
// segment bytes first. One that is not running under the policy is refused
// with *PolicyError; one that holds corrupt data is refused with
// *IntegrityError, which also retires the handle.
func Register[T any](db *DB, spec Spec) (*Collection[T], error) {
	if db == nil {
		return nil, ErrClosed
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("%w: empty name", ErrInvalidSpec)
	}
	if !inCatalogue(spec.Name) {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCollection, spec.Name)
	}
	if spec.Version < 1 {
		return nil, fmt.Errorf("%w: %q version %d", ErrInvalidSpec, spec.Name, spec.Version)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.usableLocked(); err != nil {
		return nil, err
	}
	if _, dup := db.entries[spec.Name]; dup {
		return nil, fmt.Errorf("%w: %q", ErrDuplicateCollection, spec.Name)
	}
	col, err := db.handle.Collection(spec.Name, collectionOptions(spec.Options, db.compaction)...)
	if err != nil {
		if ie := db.mon.engineIntegrity(StageRegister, spec.Name, err); ie != nil {
			return nil, db.mon.fail(ie)
		}
		return nil, fmt.Errorf("wardendb: open collection %q: %w", spec.Name, err)
	}
	if err := checkPolicy(spec.Name, col.Config(), db.compaction); err != nil {
		var pe *PolicyError
		errors.As(err, &pe)
		db.mon.emit(ActionPolicyRefused, spec.Name, map[string]string{"setting": pe.Setting, "got": pe.Got, "want": pe.Want})
		return nil, err
	}
	if ie := db.mon.verifyCollection(col); ie != nil {
		return nil, db.mon.fail(ie)
	}
	e := &entry{spec: spec, typ: reflect.TypeFor[T](), col: col}
	db.entries[spec.Name] = e
	db.order = append(db.order, spec.Name)
	db.steps = append(db.steps, closeStep{label: "collection:" + spec.Name, fn: func() error {
		db.mu.Lock()
		e.closed = true
		db.mu.Unlock()
		return nil
	}})
	db.mon.emit(ActionRegister, spec.Name, map[string]string{"version": strconv.Itoa(spec.Version)})
	return &Collection[T]{db: db, e: e}, nil
}

// Lookup returns the typed accessor of an already registered collection. It
// fails with ErrNotRegistered when the collection was never registered and
// with ErrTypeMismatch when it was registered with a different record type.
func Lookup[T any](db *DB, name string) (*Collection[T], error) {
	if db == nil {
		return nil, ErrClosed
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if err := db.usableLocked(); err != nil {
		return nil, err
	}
	e, ok := db.entries[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotRegistered, name)
	}
	if want := reflect.TypeFor[T](); e.typ != want {
		return nil, fmt.Errorf("%w: %q is %s, not %s", ErrTypeMismatch, name, e.typ, want)
	}
	if e.closed {
		return nil, ErrClosed
	}
	return &Collection[T]{db: db, e: e}, nil
}

// Registered lists the registered collections in registration order.
func (db *DB) Registered() ([]CollectionInfo, error) {
	if db == nil {
		return nil, ErrClosed
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closing || db.handle == nil {
		return nil, ErrClosed
	}
	out := make([]CollectionInfo, 0, len(db.order))
	for _, name := range db.order {
		e := db.entries[name]
		out = append(out, CollectionInfo{Name: name, Version: e.spec.Version, RecordType: e.typ.String()})
	}
	return out, nil
}

// Name returns the collection name.
func (c *Collection[T]) Name() string { return c.e.spec.Name }

// Version returns the registered record-schema version.
func (c *Collection[T]) Version() int { return c.e.spec.Version }

// Engine returns the underlying engine collection, ErrClosed once Close has
// retired this collection, or the *IntegrityError that retired the handle.
// Callers must re-fetch it per operation rather than retain it, so use after
// either is always caught here.
func (c *Collection[T]) Engine() (*engine.Collection, error) {
	c.db.mu.RLock()
	defer c.db.mu.RUnlock()
	if c.e.closed || c.db.handle == nil {
		return nil, ErrClosed
	}
	if err := c.db.mon.faultErr(); err != nil {
		return nil, err
	}
	return c.e.col, nil
}
