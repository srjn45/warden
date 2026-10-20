package backendstore

// Shared-handle construction (warden-db porting pattern, task
// coordination-and-registry-2). Same shape as internal/projectstore/shared.go:
// NewStoreFromDB takes the shared *wardendb.DB (no path, no scriva.Open, Close is
// a no-op), each collection is registered once with wardendb.Register[T] and its
// indexes ensured, and the store resolves the engine collection per call behind
// the same doc-level interface the legacy path uses.
//
// Legacy collection -> catalogue name:
//
//	backends          -> backends          (incl. the __settings__ singleton)
//	models            -> backend_models
//	role_tiers        -> backend_roles
//	handover_settings -> backend_settings
//	quotas            -> backend_quotas       (catalogue addition)
//	rl_cooldowns      -> backend_rl_cooldowns (catalogue addition)
//
// Records stay schema-1 shaped at registry Version 1; the schema-2 reshape
// (separating __settings__ into backend_settings) is the migration task.
// usage_snapshots / quota_impact_fences are JSONL stores owned by
// internal/backendusage and internal/capacity, not this package.
// Production wiring is untouched; NewStore keeps the standalone-dir path.

import (
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	backendsCollection            = "backends"
	modelsCollectionName          = "backend_models"
	roleTiersCollection           = "backend_roles"
	handoverCollection            = "backend_settings"
	quotasCollection              = "backend_quotas"
	rlCooldownsCollection         = "backend_rl_cooldowns"
	catalogScopesCollection       = "model_catalog_scopes"
	catalogSnapshotsCollection    = "model_catalog_snapshots"
	catalogObservationsCollection = "model_catalog_observations"
	catalogProposalsCollection    = "model_catalog_proposals"
	catalogRevisionsCollection    = "model_catalog_revisions"
	recordSchemaVersion           = 1
)

// docs is the document surface Store needs from a collection; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	GetByKey(string) (engine.Record, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
	UpdateByKey(string, map[string]any) (time.Time, error)
	DeleteByKey(string) error
}

// sharedDocs resolves the engine collection on every call.
type sharedDocs struct {
	get func() (*engine.Collection, error)
}

func (d sharedDocs) Config() engine.CollectionConfig {
	c, err := d.get()
	if err != nil {
		return engine.CollectionConfig{}
	}
	return c.Config()
}

func (d sharedDocs) Scan(f query.Filter) ([]engine.ScanResult, error) {
	c, err := d.get()
	if err != nil {
		return nil, err
	}
	return c.Scan(f)
}

func (d sharedDocs) GetByKey(k string) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.GetByKey(k)
}

func (d sharedDocs) InsertWithKey(k string, m map[string]any) (uint64, time.Time, error) {
	c, err := d.get()
	if err != nil {
		return 0, time.Time{}, err
	}
	return c.InsertWithKey(k, m)
}

func (d sharedDocs) UpdateByKey(k string, m map[string]any) (time.Time, error) {
	c, err := d.get()
	if err != nil {
		return time.Time{}, err
	}
	return c.UpdateByKey(k, m)
}

func (d sharedDocs) DeleteByKey(k string) error {
	c, err := d.get()
	if err != nil {
		return err
	}
	return c.DeleteByKey(k)
}

// CollectionSpecs declares the collections this store owns on the shared
// handle. Order is registration order.
func CollectionSpecs() []wardendb.Spec {
	names := []string{backendsCollection, modelsCollectionName, roleTiersCollection, handoverCollection, quotasCollection, rlCooldownsCollection, catalogScopesCollection, catalogSnapshotsCollection, catalogObservationsCollection, catalogProposalsCollection, catalogRevisionsCollection}
	out := make([]wardendb.Spec, 0, len(names))
	for _, n := range names {
		out = append(out, wardendb.Spec{Name: n, Version: recordSchemaVersion, Options: []scriva.CollectionOption(nil)})
	}
	return out
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string {
	return map[string][]string{
		modelsCollectionName: {"tier", "quota_scope"},
	}
}

// NewStoreFromDB builds a Store over collections registered on the shared
// warden-db handle. The caller keeps ownership of db and closes it; the
// collections must not already be registered on it. Like NewStore it seeds
// defaults into an empty registry.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	col, err := register[Backend](db, backendsCollection)
	if err != nil {
		return nil, err
	}
	modelsCol, err := register[ModelEntry](db, modelsCollectionName)
	if err != nil {
		return nil, err
	}
	rolesCol, err := register[RoleTierMapping](db, roleTiersCollection)
	if err != nil {
		return nil, err
	}
	handoverCol, err := register[HandoverSettings](db, handoverCollection)
	if err != nil {
		return nil, err
	}
	quotasCol, err := register[BackendQuota](db, quotasCollection)
	if err != nil {
		return nil, err
	}
	rlCol, err := register[RLCooldownEntry](db, rlCooldownsCollection)
	if err != nil {
		return nil, err
	}
	scopes, err := register[ActiveCatalogScope](db, catalogScopesCollection)
	if err != nil {
		return nil, err
	}
	snaps, err := register[CatalogSnapshot](db, catalogSnapshotsCollection)
	if err != nil {
		return nil, err
	}
	obs, err := register[StoredObservation](db, catalogObservationsCollection)
	if err != nil {
		return nil, err
	}
	props, err := register[CatalogProposal](db, catalogProposalsCollection)
	if err != nil {
		return nil, err
	}
	revs, err := register[CatalogRevision](db, catalogRevisionsCollection)
	if err != nil {
		return nil, err
	}
	s := &Store{col: col, modelsCol: modelsCol, rolesCol: rolesCol, handoverCol: handoverCol, quotasCol: quotasCol, rlCooldownsCol: rlCol, catalogScopesCol: scopes, catalogSnapshotsCol: snaps, catalogObservationsCol: obs, catalogProposalsCol: props, catalogRevisionsCol: revs, catalogMetrics: NewCatalogMetrics()}
	if err := s.seedDefaultsIfEmpty(); err != nil {
		return nil, err
	}
	return s, nil
}

func register[T any](db *wardendb.DB, name string) (docs, error) {
	var spec wardendb.Spec
	for _, s := range CollectionSpecs() {
		if s.Name == name {
			spec = s
		}
	}
	col, err := wardendb.Register[T](db, spec)
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range CollectionIndexes()[name] {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("backendstore: ensure %s.%s index: %w", name, f, err)
		}
	}
	return sharedDocs{get: col.Engine}, nil
}
