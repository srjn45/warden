package planbackup

import (
	"context"
	"errors"
	"fmt"

	"github.com/srjn45/warden/internal/planstore"
)

// Restorer applies a Bundle into a planstore.Store without consulting Git.
type Restorer struct {
	Store *planstore.Store
}

// Validate checks schema version and integrity hashes. It does not touch the store.
func Validate(b *Bundle) error {
	if b == nil {
		return fmt.Errorf("planbackup: nil bundle")
	}
	if b.SchemaVersion != SchemaVersion {
		return fmt.Errorf("planbackup: unsupported schema_version %d (want %d)", b.SchemaVersion, SchemaVersion)
	}
	if len(b.Entries) == 0 {
		return fmt.Errorf("planbackup: bundle has no entries")
	}
	// Recompute hashes into a copy so we do not mutate caller's sealed values
	// until we compare.
	clone := *b
	clone.Entries = append([]Entry(nil), b.Entries...)
	for i := range clone.Entries {
		e := clone.Entries[i]
		if e.Plan == nil || e.Plan.ID == "" {
			return fmt.Errorf("planbackup: entry %d missing plan id", i)
		}
		wantPlan := planstore.ComputeContentHash(e.Plan)
		if e.PlanContentHash != "" && e.PlanContentHash != wantPlan {
			return fmt.Errorf("planbackup: plan %s content hash mismatch: bundle %s != computed %s",
				e.Plan.ID, e.PlanContentHash, wantPlan)
		}
		if e.Plan.ContentHash != "" && e.Plan.ContentHash != wantPlan {
			return fmt.Errorf("planbackup: plan %s embedded content_hash mismatch", e.Plan.ID)
		}
		wantEvents := hashEvents(e.Events)
		if e.EventsHash != "" && e.EventsHash != wantEvents {
			return fmt.Errorf("planbackup: plan %s events hash mismatch", e.Plan.ID)
		}
		wantNotes := hashNotes(e.Notes)
		if e.NotesHash != "" && e.NotesHash != wantNotes {
			return fmt.Errorf("planbackup: plan %s notes hash mismatch", e.Plan.ID)
		}
		wantEntry := hashEntry(e.Plan.ID, e.Plan.Revision, wantPlan, wantEvents, wantNotes)
		if e.EntryHash != "" && e.EntryHash != wantEntry {
			return fmt.Errorf("planbackup: plan %s entry hash mismatch", e.Plan.ID)
		}
		clone.Entries[i].PlanContentHash = wantPlan
		clone.Entries[i].EventsHash = wantEvents
		clone.Entries[i].NotesHash = wantNotes
		clone.Entries[i].EntryHash = wantEntry
	}
	Seal(&clone)
	if b.BundleHash != "" && b.BundleHash != clone.BundleHash {
		return fmt.Errorf("planbackup: bundle_hash mismatch")
	}
	return nil
}

// Restore validates then applies b according to opts. Re-running the same
// bundle with ConflictSkip is idempotent when content hashes match.
func (r *Restorer) Restore(ctx context.Context, b *Bundle, opts RestoreOptions) (*RestoreResult, error) {
	if r == nil || r.Store == nil {
		return nil, fmt.Errorf("planbackup: restorer store not configured")
	}
	if opts.OnConflict == "" {
		opts.OnConflict = ConflictSkip
	}
	switch opts.OnConflict {
	case ConflictSkip, ConflictFail, ConflictOverwrite:
	default:
		return nil, fmt.Errorf("planbackup: unknown on_conflict %q", opts.OnConflict)
	}
	if err := Validate(b); err != nil {
		return nil, err
	}

	res := &RestoreResult{DryRun: opts.DryRun, Entries: make([]EntryResult, 0, len(b.Entries))}
	for _, e := range b.Entries {
		er, err := r.restoreEntry(ctx, e, opts)
		if err != nil {
			return res, err
		}
		res.Entries = append(res.Entries, er)
		if er.Outcome == OutcomeConflicted && opts.OnConflict == ConflictFail {
			return res, fmt.Errorf("planbackup: conflict on plan %s: %s", er.PlanID, er.Reason)
		}
	}
	return res, nil
}

func (r *Restorer) restoreEntry(ctx context.Context, e Entry, opts RestoreOptions) (EntryResult, error) {
	p := e.Plan
	er := EntryResult{
		PlanID:      p.ID,
		ContentHash: p.ContentHash,
		Revision:    p.Revision,
	}
	existing, err := r.Store.Get(ctx, p.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, planstore.ErrNotFound) {
		return er, err
	}

	if exists {
		// Idempotent retry: same stable ID + same definition hash + same revision.
		same := planstore.ComputeContentHash(existing) == planstore.ComputeContentHash(p) &&
			existing.Revision == p.Revision

		switch opts.OnConflict {
		case ConflictOverwrite:
			if opts.DryRun {
				er.Outcome = OutcomeValidated
				er.Reason = "would overwrite existing plan"
				return er, nil
			}
			if err := r.Store.Delete(ctx, p.ID); err != nil {
				return er, err
			}
			if err := r.applyPlan(ctx, e); err != nil {
				return er, err
			}
			er.Outcome = OutcomeOverwritten
			return er, nil
		case ConflictFail:
			if same {
				if opts.DryRun {
					er.Outcome = OutcomeValidated
					er.Reason = "identical plan already present"
					return er, nil
				}
				// Idempotent: still re-apply events/notes (no-op on dup keys).
				if err := r.applyChildren(ctx, e); err != nil {
					return er, err
				}
				er.Outcome = OutcomeSkipped
				er.Reason = "identical plan already present"
				return er, nil
			}
			er.Outcome = OutcomeConflicted
			er.Reason = fmt.Sprintf("stable id exists with different revision/hash (have rev=%d hash=%s)",
				existing.Revision, existing.ContentHash)
			return er, nil
		default: // skip
			if same {
				if opts.DryRun {
					er.Outcome = OutcomeValidated
					er.Reason = "identical plan already present"
					return er, nil
				}
				if err := r.applyChildren(ctx, e); err != nil {
					return er, err
				}
				er.Outcome = OutcomeSkipped
				er.Reason = "identical plan already present"
				return er, nil
			}
			er.Outcome = OutcomeConflicted
			er.Reason = fmt.Sprintf("stable id exists with different revision/hash (have rev=%d hash=%s)",
				existing.Revision, existing.ContentHash)
			return er, nil
		}
	}

	if opts.DryRun {
		er.Outcome = OutcomeValidated
		er.Reason = "would restore"
		return er, nil
	}
	if err := r.applyPlan(ctx, e); err != nil {
		return er, err
	}
	er.Outcome = OutcomeRestored
	return er, nil
}

func (r *Restorer) applyPlan(ctx context.Context, e Entry) error {
	p := sanitizePlan(e.Plan)
	if err := r.Store.RestorePlan(ctx, p); err != nil {
		return err
	}
	return r.applyChildren(ctx, e)
}

func (r *Restorer) applyChildren(ctx context.Context, e Entry) error {
	for _, ev := range e.Events {
		if ev == nil {
			continue
		}
		cp := *ev
		if ev.Payload != nil {
			pl := *ev.Payload
			cp.Payload = &pl
		}
		if err := r.Store.RestoreEvent(ctx, &cp); err != nil {
			return fmt.Errorf("restore event %s: %w", cp.ID, err)
		}
	}
	for _, n := range e.Notes {
		if n == nil {
			continue
		}
		cp := *n
		if err := r.Store.RestoreNote(ctx, &cp); err != nil {
			return fmt.Errorf("restore note %s: %w", cp.ID, err)
		}
	}
	return nil
}
