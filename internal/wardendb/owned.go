package wardendb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/dbtx"
)

// OwnerContract describes references on full owning records. It is validation
// metadata, never a separately persisted relationship/header representation.
type OwnerContract struct {
	AllowClosedCreates bool
	Identity           string
	Immutable          []string
	References         map[string]string // field -> parent collection
	RestrictedBy       map[string]string // child collection -> reference field
	Forbidden          []string
	Active             func(map[string]any) bool
	Encode             func(map[string]any) (map[string]any, error)
	Decode             func(map[string]any) (map[string]any, error)
	Validate           func(context.Context, *dbtx.Tx, map[string]any, map[string]any) error
}

// OwnedDocuments is the shared schema-3 mutation boundary for ordinary full
// domain records. Aggregates (Plans/tasks, pipelines/jobs) use dbtx directly.
type OwnedDocuments[T any] struct {
	mu       sync.Mutex
	db       *DB
	col      *Collection[T]
	boundary *dbtx.Boundary
	txcol    *dbtx.Coll[map[string]any]
	contract OwnerContract
}

func RegisterOwned[T any](db *DB, name string, contract OwnerContract, indexes ...string) (*OwnedDocuments[T], error) {
	if contract.Identity == "" {
		contract.Identity = "id"
	}
	c, err := Register[T](db, Spec{Name: name, Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex(contract.Identity, engine.KeyField)}})
	if err != nil {
		return nil, err
	}
	eng, err := c.Engine()
	if err != nil {
		return nil, err
	}
	for _, field := range indexes {
		if err := eng.EnsureIndex(field); err != nil {
			return nil, err
		}
	}
	handle, err := db.Handle()
	if err != nil {
		return nil, err
	}
	b, err := dbtx.New(handle)
	if err != nil {
		return nil, err
	}
	txcol, err := dbtx.Bind(b, name, dbtx.MapCodec{})
	if err != nil {
		return nil, err
	}
	return &OwnedDocuments[T]{db: db, col: c, boundary: b, txcol: txcol, contract: contract}, nil
}

func (d *OwnedDocuments[T]) canonical(body map[string]any, encode bool) (map[string]any, error) {
	transform := d.contract.Decode
	if encode {
		transform = d.contract.Encode
	}
	var err error
	if transform != nil {
		body, err = transform(body)
		if err != nil {
			return nil, err
		}
	}
	var value T
	if err := StrictRecord(body, &value, d.contract.Forbidden...); err != nil {
		return nil, err
	}
	return LosslessDocument(value)
}

func (d *OwnedDocuments[T]) Config() engine.CollectionConfig {
	col, err := d.col.Engine()
	if err != nil {
		return engine.CollectionConfig{}
	}
	return col.Config()
}
func (d *OwnedDocuments[T]) GetByKey(key string) (engine.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	col, err := d.col.Engine()
	if err != nil {
		return engine.Record{}, err
	}
	r, err := col.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	if _, err := d.canonical(r.Data, false); err != nil {
		return engine.Record{}, err
	}
	return r, nil
}
func (d *OwnedDocuments[T]) Scan(filter query.Filter) ([]engine.ScanResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	col, err := d.col.Engine()
	if err != nil {
		return nil, err
	}
	rows, err := col.Scan(filter)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if _, err := d.canonical(row.Data, false); err != nil {
			return nil, err
		}
	}
	return rows, nil
}
func (d *OwnedDocuments[T]) Exists(key string) (bool, error) {
	_, err := d.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (d *OwnedDocuments[T]) write(key string, body map[string]any, insert bool) (uint64, time.Time, error) {
	m, err := d.canonical(body, true)
	if err != nil {
		return 0, time.Time{}, err
	}
	if m[d.contract.Identity] != key {
		return 0, time.Time{}, fmt.Errorf("wardendb: immutable %s identity", d.col.Name())
	}
	if project, _ := m["project_id"].(string); project == "" {
		return 0, time.Time{}, fmt.Errorf("wardendb: %s requires direct project ownership", d.col.Name())
	}
	var numericID uint64
	_, err = d.boundary.Run(context.Background(), "owned/write/"+d.col.Name(), func(ctx context.Context, tx *dbtx.Tx) error {
		col, err := d.col.Engine()
		if err != nil {
			return err
		}
		current, readErr := col.GetByKey(key)
		if insert && readErr == nil {
			return engine.ErrDuplicateKey
		}
		if !insert && readErr != nil {
			return readErr
		}
		if insert && !errors.Is(readErr, engine.ErrKeyNotFound) {
			return readErr
		}
		if !insert {
			if _, err := d.canonical(current.Data, false); err != nil {
				return err
			}
			for _, field := range append([]string{"project_id"}, d.contract.Immutable...) {
				old, oldPresent := current.Data[field]
				next, nextPresent := m[field]
				// Omitted and empty optional roots express the same fixed root.
				if !oldPresent {
					old = ""
				}
				if !nextPresent {
					next = ""
				}
				if !reflect.DeepEqual(old, next) {
					return fmt.Errorf("wardendb: immutable %s.%s", d.col.Name(), field)
				}
			}
		}
		handle, err := d.db.Handle()
		if err != nil {
			return err
		}
		refs := map[string]string{"project_id": "projects"}
		for field, collection := range d.contract.References {
			refs[field] = collection
		}
		for field, collection := range refs {
			id, _ := m[field].(string)
			if id == "" {
				continue
			}
			parentCol, err := handle.Engine().Collection(collection)
			if err != nil {
				return err
			}
			parent, err := parentCol.GetByKey(id)
			if err != nil {
				return err
			}
			if collection == "projects" {
				newWork := insert && !d.contract.AllowClosedCreates || d.contract.Active != nil && d.contract.Active(m) && !d.contract.Active(current.Data)
				if newWork && parent.Data["status"] == "closed" {
					return ErrProjectClosed
				}
			} else if parent.Data["project_id"] != m["project_id"] {
				return fmt.Errorf("wardendb: cross-project %s reference", field)
			}
			parentTx, err := dbtx.Bind(d.boundary, collection, dbtx.MapCodec{})
			if err != nil {
				return err
			}
			if err := parentTx.Update(tx, parent.ID, parent.Data, parent.Rev); err != nil {
				return err
			}
		}
		m[engine.KeyField] = key
		if d.contract.Validate != nil {
			if err := d.contract.Validate(ctx, tx, m, current.Data); err != nil {
				return err
			}
		}
		if insert {
			numericID, err = d.txcol.Insert(tx, m)
			return err
		}
		numericID = current.ID
		return d.txcol.Update(tx, current.ID, m, current.Rev)
	})
	return numericID, time.Now().UTC(), err
}
func (d *OwnedDocuments[T]) InsertWithKey(key string, body map[string]any) (uint64, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.write(key, body, true)
}
func (d *OwnedDocuments[T]) UpdateByKey(key string, body map[string]any) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, at, err := d.write(key, body, false)
	return at, err
}
func (d *OwnedDocuments[T]) DeleteByKey(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.boundary.Run(context.Background(), "owned/delete/"+d.col.Name(), func(ctx context.Context, tx *dbtx.Tx) error {
		col, err := d.col.Engine()
		if err != nil {
			return err
		}
		r, err := col.GetByKey(key)
		if err != nil {
			return err
		}
		handle, err := d.db.Handle()
		if err != nil {
			return err
		}
		for name, field := range d.contract.RestrictedBy {
			children, err := handle.Engine().Collection(name)
			if err != nil {
				return err
			}
			rows, err := children.Scan(query.MatchAll)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Data[field] == key {
					return ErrReferenced
				}
			}
		}
		return d.txcol.Delete(tx, r.ID, r.Rev)
	})
	return err
}
