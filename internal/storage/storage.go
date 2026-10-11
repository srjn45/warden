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

// Open selects compatibility mode for schema-1 data.  A schema-2 ledger, or
// an already-created warden-db directory, selects shared mode.  The latter is
// intentionally a non-user-facing test/migration marker: this binary still
// writes schema-1 ledgers for real fresh data directories.
func Open(dataDir string, opts ...wardendb.Option) (*Stores, error) {
	shared, err := sharedMode(dataDir)
	if err != nil {
		return nil, err
	}
	if !shared {
		return openCompat(dataDir)
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
	if s.Audit, err = auditstore.NewStoreFromDB(db); err != nil {
		return fail(err)
	}
	return s, nil
}

func sharedMode(dataDir string) (bool, error) {
	l, err := schema.Load(dataDir)
	if err == nil {
		return l.SchemaVersion >= wardendb.SchemaVersion, nil
	}
	if !errors.Is(err, schema.ErrNoLedger) {
		return false, fmt.Errorf("storage: schema state: %w", err)
	}
	p, err := wardendb.Probe(dataDir)
	return p.Exists, err
}
