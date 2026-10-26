package projectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/wardendb"
)

type projectHeader Project
type groupHeader ProjectGroup

type schema3Projects struct {
	mu         *sync.Mutex
	db         *wardendb.DB
	native     docs
	name       string
	boundary   *dbtx.Boundary
	collection *dbtx.Coll[map[string]any]
}

func NewSchema3StoreFromDB(db *wardendb.DB) (*Store, error) {
	gate := &sync.Mutex{}
	open := func(name string, register func() (docs, error)) (*schema3Projects, error) {
		native, err := register()
		if err != nil {
			return nil, err
		}
		handle, err := db.Handle()
		if err != nil {
			return nil, err
		}
		b, err := dbtx.New(handle)
		if err != nil {
			return nil, err
		}
		col, err := dbtx.Bind(b, name, dbtx.MapCodec{})
		if err != nil {
			return nil, err
		}
		return &schema3Projects{mu: gate, db: db, native: native, name: name, boundary: b, collection: col}, nil
	}
	p, err := open(projectsCollection, func() (docs, error) {
		return registerSchema3[projectHeader](db, projectsCollection, "status", "group_id")
	})
	if err != nil {
		return nil, err
	}
	g, err := open(groupsCollection, func() (docs, error) { return registerSchema3[groupHeader](db, groupsCollection, "name") })
	if err != nil {
		return nil, err
	}
	return &Store{col: p, groups: g, now: time.Now}, nil
}

func registerSchema3[T any](db *wardendb.DB, name string, fields ...string) (docs, error) {
	c, err := wardendb.Register[T](db, wardendb.Spec{Name: name, Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id", engine.KeyField)}})
	if err != nil {
		return nil, err
	}
	eng, err := c.Engine()
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		if err := eng.EnsureIndex(field); err != nil {
			return nil, err
		}
	}
	return sharedDocs{get: c.Engine}, nil
}

func (d *schema3Projects) Config() engine.CollectionConfig { return d.native.Config() }
func (d *schema3Projects) childRows(collection, field, owner string) ([]engine.ScanResult, error) {
	handle, err := d.db.Handle()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(handle.Engine().ListCollections(), collection) {
		return nil, nil
	}
	col, err := handle.Engine().Collection(collection)
	if err != nil {
		return nil, err
	}
	value, _ := json.Marshal(owner)
	return col.Scan(&query.FieldFilter{Field: field, Op: query.OpEq, Value: string(value)})
}

func (d *schema3Projects) hydrate(r engine.Record) (engine.Record, error) {
	if d.name == groupsCollection {
		var group groupHeader
		if err := wardendb.StrictRecord(r.Data, &group, "project_ids"); err != nil {
			return engine.Record{}, err
		}
		rows, err := d.childRows(projectsCollection, "group_id", group.ID)
		if err != nil {
			return engine.Record{}, err
		}
		sort.Slice(rows, func(i, j int) bool {
			return query.Compare(rows[i].Data["group_order"], rows[j].Data["group_order"]) < 0
		})
		group.ProjectIDs = make([]string, 0, len(rows))
		for _, row := range rows {
			id, _ := row.Data["id"].(string)
			group.ProjectIDs = append(group.ProjectIDs, id)
		}
		r.Data, err = wardendb.LosslessDocument(group)
		return r, err
	}
	var project projectHeader
	if err := wardendb.StrictRecord(r.Data, &project, "agents", "pipelines", "plans", "terminals", "autopilots"); err != nil {
		return engine.Record{}, err
	}
	for _, member := range []struct {
		collection, field, identity string
		dst                         *[]string
	}{
		{"agents", "project_id", "id", &project.Agents}, {"pipelines", "project_id", "id", &project.Pipelines},
		{"plans", "project_id", "id", &project.Plans}, {"terminals", "project_id", "id", &project.Terminals}, {"autopilot_runs", "project_id", "run_id", &project.Autopilots},
	} {
		rows, err := d.childRows(member.collection, member.field, project.ID)
		if err != nil {
			return engine.Record{}, err
		}
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			id, _ := row.Data[member.identity].(string)
			ids = append(ids, id)
		}
		sort.Strings(ids)
		*member.dst = ids
	}
	var err error
	r.Data, err = wardendb.LosslessDocument(project)
	return r, err
}

func (d *schema3Projects) GetByKey(key string) (engine.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.native.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	return d.hydrate(r)
}
func (d *schema3Projects) Scan(f query.Filter) ([]engine.ScanResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.native.Scan(f)
	if err != nil {
		return nil, err
	}
	for i, row := range rows {
		r, err := d.hydrate(engine.Record{ID: row.ID, Rev: row.Rev, Data: row.Data})
		if err != nil {
			return nil, err
		}
		rows[i].Data = r.Data
	}
	return rows, nil
}

func (d *schema3Projects) canonical(key string, body map[string]any, current *engine.Record) (map[string]any, error) {
	if d.name == groupsCollection {
		var group ProjectGroup
		if err := wardendb.StrictRecord(body, &group); err != nil {
			return nil, err
		}
		if group.ID != key {
			return nil, fmt.Errorf("projectstore: immutable group identity")
		}
		var derived []string
		if current != nil {
			view, err := d.hydrate(*current)
			if err != nil {
				return nil, err
			}
			var old ProjectGroup
			if err := wardendb.StrictRecord(view.Data, &old); err != nil {
				return nil, err
			}
			derived = old.ProjectIDs
		}
		if len(group.ProjectIDs) != 0 || len(derived) != 0 {
			if !reflect.DeepEqual(group.ProjectIDs, derived) {
				return nil, fmt.Errorf("projectstore: group membership is immutable on projects")
			}
		}
		m, err := wardendb.LosslessDocument(groupHeader(group))
		if err != nil {
			return nil, err
		}
		delete(m, "project_ids")
		return m, nil
	}
	var project Project
	if err := wardendb.StrictRecord(body, &project); err != nil {
		return nil, err
	}
	if project.ID != key {
		return nil, fmt.Errorf("projectstore: immutable project identity")
	}
	if current != nil {
		var old projectHeader
		if err := wardendb.StrictRecord(current.Data, &old, "agents", "pipelines", "plans", "terminals", "autopilots"); err != nil {
			return nil, err
		}
		if old.GroupID != project.GroupID || !reflect.DeepEqual(old.GroupOrder, project.GroupOrder) {
			return nil, fmt.Errorf("projectstore: immutable group ownership/order")
		}
	}
	if project.GroupID == "" && project.GroupOrder != nil || project.GroupID != "" && (project.GroupOrder == nil || *project.GroupOrder < 0) {
		return nil, fmt.Errorf("projectstore: invalid group ordering")
	}
	rows, err := d.native.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Data["id"] == project.ID {
			continue
		}
		if project.Path != "" && row.Data["path"] == project.Path {
			return nil, fmt.Errorf("projectstore: nonempty path already belongs to another project")
		}
		if project.GroupID != "" && row.Data["group_id"] == project.GroupID && query.Compare(row.Data["group_order"], *project.GroupOrder) == 0 {
			return nil, fmt.Errorf("projectstore: duplicate group order")
		}
	}
	m, err := wardendb.LosslessDocument(projectHeader(project))
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"agents", "pipelines", "plans", "terminals", "autopilots"} {
		delete(m, field)
	}
	return m, nil
}

func (d *schema3Projects) write(key string, body map[string]any, insert bool) (uint64, time.Time, error) {
	var numericID uint64
	_, err := d.boundary.Run(context.Background(), "projectstore/schema3-write", func(ctx context.Context, tx *dbtx.Tx) error {
		r, err := d.native.GetByKey(key)
		if insert && err == nil {
			return engine.ErrDuplicateKey
		}
		if !insert && err != nil {
			return err
		}
		if insert && !errors.Is(err, engine.ErrKeyNotFound) {
			return err
		}
		var old *engine.Record
		if !insert {
			old = &r
		}
		m, err := d.canonical(key, body, old)
		if err != nil {
			return err
		}
		if d.name == projectsCollection {
			if groupID, _ := m["group_id"].(string); groupID != "" {
				handle, err := d.db.Handle()
				if err != nil {
					return err
				}
				groups, err := handle.Engine().Collection(groupsCollection)
				if err != nil {
					return err
				}
				group, err := groups.GetByKey(groupID)
				if err != nil {
					return err
				}
				groupsTx, err := dbtx.Bind(d.boundary, groupsCollection, dbtx.MapCodec{})
				if err != nil {
					return err
				}
				if err := groupsTx.Update(tx, group.ID, group.Data, group.Rev); err != nil {
					return err
				}
			}
		}
		m[engine.KeyField] = key
		if insert {
			numericID, err = d.collection.Insert(tx, m)
			return err
		}
		numericID = r.ID
		return d.collection.Update(tx, r.ID, m, r.Rev)
	})
	return numericID, time.Now().UTC(), err
}
func (d *schema3Projects) InsertWithKey(key string, body map[string]any) (uint64, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.write(key, body, true)
}
func (d *schema3Projects) UpdateByKey(key string, body map[string]any) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, at, err := d.write(key, body, false)
	return at, err
}
func (d *schema3Projects) DeleteByKey(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.boundary.Run(context.Background(), "projectstore/schema3-delete", func(ctx context.Context, tx *dbtx.Tx) error {
		r, err := d.native.GetByKey(key)
		if err != nil {
			return err
		}
		children := []string{"agents", "terminals", "plans", "pipelines", "pipeline_jobs", "plan_tasks", "autopilot_runs"}
		field := "project_id"
		if d.name == groupsCollection {
			children = []string{projectsCollection}
			field = "group_id"
		}
		for _, collection := range children {
			rows, err := d.childRows(collection, field, key)
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				return wardendb.ErrReferenced
			}
		}
		return d.collection.Delete(tx, r.ID, r.Rev)
	})
	return err
}
