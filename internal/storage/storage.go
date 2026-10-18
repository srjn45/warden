// Package storage is the daemon storage composition root.
//
// It is deliberately the only production package that selects between the
// schema-1 compatibility layout and the schema-2 shared warden-db layout.
// The compatibility implementation is isolated in compat.go and is deleted by
// the schema-2 cutover plan; callers only depend on Stores.
package storage

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/audit/auditstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/knownprompts"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/srjn45/warden/internal/wardendb"
)

// Stores is every durable daemon dependency.  Close is the single ownership
// boundary; daemon wiring must neither open nor close an individual store.
type Stores struct {
	DB             *wardendb.DB // non-nil only in shared schema-2 mode
	Agents         *agentstore.Store
	Terminals      *terminalstore.Store
	Context        *ctxstore.Store
	Mailbox        *mailbox.Store
	KnownPrompts   *knownprompts.Store
	Pipelines      *pipeline.Store
	Snapshots      *snapshot.Store
	Savings        *savings.Store
	Spend          *spend.Store
	Schedules      *schedule.Store
	Backends       *backendstore.Store
	UsageSnapshots *backendusage.SnapshotStore
	ImpactFences   *capacity.DurableFenceStore
	Projects       *projectstore.Store
	Plans          *planstore.Store
	PlanExports    *planexport.Store
	Metrics        metrics.SampleStore
	AutopilotRuns  *autopilot.RunStore
	Autopilots     *autopilotstore.Store
	Audit          audit.Trail
	close          func() error
}

func (s *Stores) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	return s.close()
}

// Open opens the single canonical warden-db (schema 2). It never falls back to
// the schema-1 stores: data that is still on them (a schema-1 ledger, an
// interrupted migration, or legacy stores with no ledger) is refused with the
// boot guard's error so the operator runs the journaled cutover instead of
// the process silently running a second, divergent store set.
func Open(dataDir string, opts ...wardendb.Option) (*Stores, error) {
	if err := requireSharedMode(dataDir); err != nil {
		return nil, err
	}
	db, err := wardendb.Open(dataDir, opts...)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Stores, error) { _ = db.Close(); return nil, err }
	s := &Stores{DB: db, close: db.Close}
	if s.Agents, err = agentstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if err = db.OnClose("agentstore", s.Agents.Close); err != nil {
		return fail(err)
	}
	if s.Terminals, err = terminalstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Context, err = ctxstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Mailbox, err = mailbox.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.KnownPrompts, err = knownprompts.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Pipelines, err = pipeline.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Snapshots, err = snapshot.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Savings, err = savings.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Spend, err = spend.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Schedules, err = schedule.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Backends, err = backendstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.UsageSnapshots, err = backendusage.NewSnapshotStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.ImpactFences, err = capacity.NewDurableFenceStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Projects, err = projectstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Plans, err = planstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.PlanExports, err = planexport.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Metrics, err = metrics.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.AutopilotRuns, err = autopilot.NewRunStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.Autopilots, err = autopilotstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	// Queryable events live in warden-db; audit.jsonl stays the append-only
	// external stream (migration contract): keep appending to it so the files
	// every operator tool reads never go stale after the cutover.
	auditDB, err := auditstore.NewStoreFromDB(db)
	if err != nil {
		return fail(err)
	}
	s.Audit = audit.NewTee(auditDB, audit.NewWriter(filepath.Join(dataDir, "audit.jsonl")))
	return s, nil
}

// OpenLegacy opens the schema-1 per-store layout. It exists for offline
// tooling and for fixtures that must produce authentic schema-1 data for the
// cutover; no daemon path calls it. It refuses a data dir that is (becoming)
// schema 2, so it can never create a second writer beside warden-db.
func OpenLegacy(dataDir string) (*Stores, error) {
	l, err := schema.Load(dataDir)
	switch {
	case err == nil && (l.SchemaVersion >= schema.Schema2 || l.MigrationInterrupted()):
		return nil, &schema.GuardError{Verdict: schema.VerdictDataNewer, DataDir: dataDir, Ledger: *l, Binary: schema.LegacyBaseline, Min: schema.MinSchema}
	case err != nil && !errors.Is(err, schema.ErrNoLedger):
		return nil, fmt.Errorf("storage: schema state: %w", err)
	}
	if schema.WardenDBPresent(dataDir) {
		return nil, &schema.GuardError{Verdict: schema.VerdictSchema2Residue, DataDir: dataDir, Binary: schema.LegacyBaseline, Min: schema.MinSchema}
	}
	return openCompat(dataDir)
}

// requireSharedMode proves dataDir is safe to open as warden-db and returns the
// boot guard's refusal when it is not.
func requireSharedMode(dataDir string) error {
	l, err := schema.Load(dataDir)
	if err == nil {
		switch {
		case l.MigrationInterrupted():
			// Never open stores in the middle of a migration, whichever side of
			// the promotion it stopped on.
			return &schema.GuardError{Verdict: schema.VerdictInterrupted, DataDir: dataDir, Ledger: *l, Binary: schema.SchemaVersion, Min: schema.MinSchema}
		case l.SchemaVersion >= wardendb.SchemaVersion:
			return nil
		case schema.WardenDBPresent(dataDir):
			// Two candidate authorities: never fall back to the legacy stores
			// (nor adopt warden-db) on a guess. The operator decides.
			return &schema.GuardError{Verdict: schema.VerdictSchema2Residue, DataDir: dataDir, Ledger: *l, Binary: schema.SchemaVersion, Min: schema.MinSchema}
		}
		return &schema.GuardError{Verdict: schema.VerdictNeedsMigration, DataDir: dataDir, Ledger: *l, Binary: schema.SchemaVersion, Min: schema.MinSchema}
	}
	if !errors.Is(err, schema.ErrNoLedger) {
		return fmt.Errorf("storage: schema state: %w", err)
	}
	// No ledger. An existing warden-db is the canonical store; a data dir with
	// no data at all is a fresh install and starts directly on schema 2; legacy
	// stores without a ledger need the cutover (the boot guard stamps them at
	// the schema-1 baseline first).
	if p, err := wardendb.Probe(dataDir); err != nil {
		return err
	} else if p.Exists {
		return nil
	}
	if b := schema.InferBaseline(dataDir); !b.Fresh {
		return &schema.GuardError{Verdict: schema.VerdictNeedsMigration, DataDir: dataDir,
			Ledger: schema.Ledger{SchemaVersion: b.Version}, Binary: schema.SchemaVersion, Min: schema.MinSchema}
	}
	return nil
}
