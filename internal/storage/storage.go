// Package storage is the daemon storage composition root.
//
// It is deliberately the only production package that selects between the
// schema-1 compatibility layout and the schema-2 shared warden-db layout.
// The compatibility implementation is isolated in compat.go and is deleted by
// the schema-2 cutover plan; callers only depend on Stores.
package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/srjn45/warden/internal/adoption"
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
	"github.com/srjn45/warden/internal/migrate/schema3"
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

// Open opens the single canonical warden-db. It never falls back to
// the schema-1 stores: data that is still on them (a schema-1 ledger, an
// interrupted migration, or legacy stores with no ledger) is refused with the
// boot guard's error so the operator runs the journaled cutover instead of
// the process silently running a second, divergent store set.
func Open(dataDir string, opts ...wardendb.Option) (*Stores, error) {
	if err := requireSharedMode(dataDir); err != nil {
		return nil, err
	}
	l, err := schema.Load(dataDir)
	if err != nil {
		return nil, err
	}
	return openShared(dataDir, l.SchemaVersion == schema.SchemaVersion, l.MigrationInterrupted(), opts...)
}

// OpenSchema2ForMigrationVerification opens an exact, committed schema-2
// generation with its historical codecs. It is for offline migration
// equivalence/retirement verification only; daemon composition must use Open,
// which refuses schema 2 once format 3 is active.
func OpenSchema2ForMigrationVerification(dataDir string, opts ...wardendb.Option) (*Stores, error) {
	l, err := schema.Load(dataDir)
	if err != nil {
		return nil, err
	}
	if l.MigrationInterrupted() {
		return nil, &schema.GuardError{Verdict: schema.VerdictInterrupted, DataDir: dataDir, Ledger: *l, Binary: schema.Schema2, Min: 1}
	}
	if l.SchemaVersion != schema.Schema2 {
		return nil, fmt.Errorf("schema-2 verification requires exact committed schema 2")
	}
	return openShared(dataDir, false, false, opts...)
}

func openShared(dataDir string, schema3Mode, sealedReadiness bool, opts ...wardendb.Option) (*Stores, error) {
	db, err := wardendb.Open(dataDir, opts...)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Stores, error) { _ = db.Close(); return nil, err }
	if schema3Mode {
		g, _, err := schema3.Read(context.Background(), wardendb.Dir(dataDir))
		if err != nil {
			return fail(fmt.Errorf("storage: read schema-3 generation: %w", err))
		}
		if err := schema3.ValidateTarget(g); err != nil {
			return fail(fmt.Errorf("storage: validate schema-3 generation: %w", err))
		}
	}
	s := &Stores{DB: db, close: db.Close}
	if schema3Mode {
		s.Agents, err = agentstore.NewSchema3StoreFromDB(db)
	} else {
		s.Agents, err = agentstore.NewStoreFromDB(db)
	}
	if err != nil {
		return fail(err)
	}
	if err = db.OnClose("agentstore", s.Agents.Close); err != nil {
		return fail(err)
	}
	if schema3Mode {
		s.Terminals, err = terminalstore.NewSchema3StoreFromDB(db)
	} else {
		s.Terminals, err = terminalstore.NewStoreFromDB(db)
	}
	if err != nil {
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
	if schema3Mode {
		s.Pipelines, err = pipeline.NewSchema3StoreFromDB(db)
	} else {
		s.Pipelines, err = pipeline.NewStoreFromDB(db)
	}
	if err != nil {
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
	if schema3Mode && sealedReadiness {
		s.Backends, err = backendstore.NewSchema3StoreFromDB(db)
	} else {
		s.Backends, err = backendstore.NewStoreFromDB(db)
	}
	if err != nil {
		return fail(err)
	}
	if s.UsageSnapshots, err = backendusage.NewSnapshotStoreFromDB(db); err != nil {
		return fail(err)
	}
	if s.ImpactFences, err = capacity.NewDurableFenceStoreFromDB(db); err != nil {
		return fail(err)
	}
	if schema3Mode {
		s.Projects, err = projectstore.NewSchema3StoreFromDB(db)
	} else {
		s.Projects, err = projectstore.NewStoreFromDB(db)
	}
	if err != nil {
		return fail(err)
	}
	if schema3Mode {
		s.Plans, err = planstore.NewSchema3StoreFromDB(db)
	} else {
		s.Plans, err = planstore.NewStoreFromDB(db)
	}
	if err != nil {
		return fail(err)
	}
	if schema3Mode {
		s.PlanExports, err = planexport.NewSchema3StoreFromDB(db)
	} else {
		s.PlanExports, err = planexport.NewStoreFromDB(db)
	}
	if err != nil {
		return fail(err)
	}
	if s.Metrics, err = metrics.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	if schema3Mode {
		s.AutopilotRuns, err = autopilot.NewSchema3RunStoreFromDB(db)
	} else {
		s.AutopilotRuns, err = autopilot.NewRunStoreFromDB(db)
	}
	if err != nil {
		return fail(err)
	}
	if !schema3Mode {
		if s.Autopilots, err = autopilotstore.NewStoreFromDB(db); err != nil {
			return fail(err)
		}
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
		if l.SchemaVersion >= schema.Schema2 {
			_ = adoption.Record(dataDir, adoption.Event{Kind: adoption.LegacyOpenRefused, ToSchema: l.SchemaVersion})
		}
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
			if err := schema.PendingSchema3TargetAuthorized(dataDir, l); err == nil {
				return nil
			}
			// Never open stores in the middle of a migration, whichever side of
			// the promotion it stopped on.
			return &schema.GuardError{Verdict: schema.VerdictInterrupted, DataDir: dataDir, Ledger: *l, Binary: schema.SchemaVersion, Min: schema.MinSchema}
		case l.SchemaVersion > wardendb.SchemaVersion:
			return &schema.GuardError{Verdict: schema.VerdictDataNewer, DataDir: dataDir, Ledger: *l, Binary: schema.SchemaVersion, Min: schema.MinSchema}
		case l.SchemaVersion == wardendb.SchemaVersion:
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
	// An existing DB without its format ledger is ambiguous. The composition
	// root must enforce the same refusal as daemon boot before registration can
	// create collections or let a schema-2 codec rewrite schema-3 records.
	if p, err := wardendb.Probe(dataDir); err != nil {
		return err
	} else if p.Exists {
		return &schema.GuardError{Verdict: schema.VerdictSchema2Residue, DataDir: dataDir, Binary: schema.SchemaVersion, Min: schema.MinSchema}
	}
	if b := schema.InferBaseline(dataDir); !b.Fresh {
		return &schema.GuardError{Verdict: schema.VerdictNeedsMigration, DataDir: dataDir,
			Ledger: schema.Ledger{SchemaVersion: b.Version}, Binary: schema.SchemaVersion, Min: schema.MinSchema}
	}
	// Stamp only a proven fresh install so a later direct Open cannot mistake
	// its database for a ledger-less generation. No existing generation is adopted.
	_, _, err = schema.Ensure(dataDir, "")
	return err
}
