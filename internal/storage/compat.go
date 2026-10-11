package storage

// Schema-1 compatibility adapter.  Delete this file in the schema-2 cutover.
// Keep every legacy path and opener here, rather than letting daemon startup
// own them piecemeal.

import (
	"errors"
	"path/filepath"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
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
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/terminalstore"
)

func openCompat(d string) (*Stores, error) {
	s := &Stores{}
	var closers []func() error
	add := func(c func() error) { closers = append(closers, c) }
	fail := func(err error) (*Stores, error) {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i]()
		}
		return nil, err
	}
	var err error
	if s.Agents, err = agentstore.New(d); err != nil {
		return fail(err)
	}
	add(s.Agents.Close)
	if s.Terminals, err = terminalstore.New(d); err != nil {
		return fail(err)
	}
	add(s.Terminals.Close)
	if s.Context, err = ctxstore.New(filepath.Join(d, "context")); err != nil {
		return fail(err)
	}
	add(s.Context.Close)
	if s.Mailbox, err = mailbox.New(filepath.Join(d, "inbox")); err != nil {
		return fail(err)
	}
	add(s.Mailbox.Close)
	if s.KnownPrompts, err = knownprompts.New(d); err != nil {
		return fail(err)
	}
	add(s.KnownPrompts.Close)
	if s.Pipelines, err = pipeline.NewStore(filepath.Join(d, "pipelines")); err != nil {
		return fail(err)
	}
	add(s.Pipelines.Close)
	if s.Snapshots, err = snapshot.NewStore(filepath.Join(d, "snapshots")); err != nil {
		return fail(err)
	}
	add(s.Snapshots.Close)
	if s.Savings, err = savings.NewStore(filepath.Join(d, "savings")); err != nil {
		return fail(err)
	}
	if s.Spend, err = spend.NewStore(filepath.Join(d, "spend")); err != nil {
		return fail(err)
	}
	if s.Schedules, err = schedule.NewStore(filepath.Join(d, "schedules.json")); err != nil {
		return fail(err)
	}
	add(s.Schedules.Close)
	if s.Backends, _, err = backendstore.Open(filepath.Join(d, "backends"), backendstore.Options{}); err != nil {
		return fail(err)
	}
	add(s.Backends.Close)
	if s.UsageSnapshots, err = backendusage.NewSnapshotStore(filepath.Join(d, "usage-snapshots")); err != nil {
		return fail(err)
	}
	if s.ImpactFences, err = capacity.NewDurableFenceStore(filepath.Join(d, "quota-impact")); err != nil {
		return fail(err)
	}
	if s.Projects, err = projectstore.NewStore(filepath.Join(d, "projects")); err != nil {
		return fail(err)
	}
	add(s.Projects.Close)
	if s.Plans, err = planstore.New(filepath.Join(d, "plans")); err != nil {
		return fail(err)
	}
	add(s.Plans.Close)
	if s.PlanExports, err = planexport.NewStore(filepath.Join(d, "plans")); err != nil {
		return fail(err)
	}
	add(s.PlanExports.Close)
	if s.Metrics, err = metrics.NewRecorder(filepath.Join(d, "metrics")); err != nil {
		return fail(err)
	}
	if s.AutopilotRuns, err = autopilot.NewRunStore(d); err != nil {
		return fail(err)
	}
	add(s.AutopilotRuns.Close)
	if s.Autopilots, err = autopilotstore.New(d); err != nil {
		return fail(err)
	}
	add(s.Autopilots.Close)
	s.Audit = audit.NewWriter(filepath.Join(d, "audit.jsonl"))
	s.close = func() error {
		var es []error
		for i := len(closers) - 1; i >= 0; i-- {
			es = append(es, closers[i]())
		}
		return errors.Join(es...)
	}
	return s, nil
}
