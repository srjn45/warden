package planbackup

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// Exporter builds Plan backup bundles from a planstore.Store.
type Exporter struct {
	Store *planstore.Store
	// Now, when set, stamps ExportedAt (tests). Default: time.Now().UTC().
	Now func() time.Time
}

func (e *Exporter) now() time.Time {
	if e != nil && e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// ExportOptions selects which Plans to include.
type ExportOptions struct {
	// PlanIDs, when non-empty, exports only those IDs (order preserved after
	// stable sort by ID for hashing).
	PlanIDs []string
	// ProjectID, when set with All, limits the export to one project.
	ProjectID string
	// All exports every Plan in the store (optionally filtered by ProjectID).
	All bool
}

// Export builds a sealed Bundle. It never reads Git or plans/ replicas.
func (e *Exporter) Export(ctx context.Context, opts ExportOptions) (*Bundle, error) {
	if e == nil || e.Store == nil {
		return nil, fmt.Errorf("planbackup: exporter store not configured")
	}
	plans, err := e.selectPlans(ctx, opts)
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return nil, fmt.Errorf("planbackup: no plans selected for export")
	}

	b := &Bundle{
		SchemaVersion: SchemaVersion,
		ExportedAt:    e.now(),
		Entries:       make([]Entry, 0, len(plans)),
	}
	for _, p := range plans {
		events, err := e.Store.ListAllEvents(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("planbackup: list events for %s: %w", p.ID, err)
		}
		notes, err := e.Store.ListAllNotes(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("planbackup: list notes for %s: %w", p.ID, err)
		}
		b.Entries = append(b.Entries, Entry{
			Plan:   sanitizePlan(p),
			Events: cloneEvents(events),
			Notes:  cloneNotes(notes),
		})
	}
	sort.Slice(b.Entries, func(i, j int) bool {
		return b.Entries[i].Plan.ID < b.Entries[j].Plan.ID
	})
	Seal(b)
	return b, nil
}

func (e *Exporter) selectPlans(ctx context.Context, opts ExportOptions) ([]*planstore.Plan, error) {
	if len(opts.PlanIDs) > 0 {
		out := make([]*planstore.Plan, 0, len(opts.PlanIDs))
		seen := map[string]bool{}
		for _, id := range opts.PlanIDs {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			p, err := e.Store.Get(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("planbackup: get %s: %w", id, err)
			}
			out = append(out, p)
		}
		return out, nil
	}
	if !opts.All {
		return nil, fmt.Errorf("planbackup: specify plan ids or all=true")
	}
	all, err := e.Store.List(ctx)
	if err != nil {
		return nil, err
	}
	if opts.ProjectID == "" {
		return all, nil
	}
	var out []*planstore.Plan
	for _, p := range all {
		if p.ProjectID == opts.ProjectID {
			out = append(out, p)
		}
	}
	return out, nil
}
