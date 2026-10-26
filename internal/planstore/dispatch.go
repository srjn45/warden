package planstore

import (
	"context"
	"fmt"
	"reflect"

	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/wardendb"
)

var ErrDispatchConflict = fmt.Errorf("planstore: dispatch compare-and-set conflict")

// TaskDispatches reads current execution dispatch from its sole task-row
// authority. Historical dispatch is read from ExecutionHistory instead.
func (s *Store) TaskDispatches(ctx context.Context, planID, executionID string) (map[string]TaskDispatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d, ok := s.col.(*schema3Plans)
	if !ok {
		return nil, fmt.Errorf("planstore: materialized task authority required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dispatches(planID, executionID)
}

func (d *schema3Plans) dispatches(planID, executionID string) (map[string]TaskDispatch, error) {
	r, err := d.header.GetByKey(planID)
	if err != nil {
		return nil, err
	}
	var plan Plan
	if err := wardendb.StrictRecord(r.Data, &plan, "tasks", "task_progress", "task_outcomes", "pipeline_id", "autopilot_run_id", "orchestrator_id"); err != nil {
		return nil, err
	}
	if plan.ActiveExecution == nil || executionID == "" || plan.ActiveExecution.ID != executionID {
		return nil, fmt.Errorf("planstore: dispatch requires the selected current execution")
	}
	_, rows, err := d.taskRows(planID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]TaskDispatch)
	for _, task := range rows {
		if task.Dispatch != nil {
			if task.Dispatch.ExecutionID != executionID {
				return nil, fmt.Errorf("planstore: dispatch belongs to another execution")
			}
			out[task.TaskID] = *task.Dispatch
		}
	}
	return out, nil
}

// CompareTaskDispatches changes operational dispatch only. In particular,
// dispatch state landed never changes the Plan task's lifecycle to done.
func (s *Store) CompareTaskDispatches(ctx context.Context, planID, executionID string, expected, next map[string]TaskDispatch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d, ok := s.col.(*schema3Plans)
	if !ok {
		return fmt.Errorf("planstore: materialized task authority required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.boundary.Run(ctx, "planstore/task-dispatch", func(ctx context.Context, tx *dbtx.Tx) error {
		current, err := d.dispatches(planID, executionID)
		if err != nil {
			return err
		}
		if !(len(current) == 0 && len(expected) == 0) && !reflect.DeepEqual(current, expected) {
			return ErrDispatchConflict
		}
		parent, err := d.header.GetByKey(planID)
		if err != nil {
			return err
		}
		if err := d.plansTx.Update(tx, parent.ID, parent.Data, parent.Rev); err != nil {
			return err
		}
		raw, rows, err := d.taskRows(planID)
		if err != nil {
			return err
		}
		known := make(map[string]TaskRecord, len(rows))
		for _, row := range rows {
			known[row.TaskID] = row
		}
		for id, dispatch := range next {
			if _, found := known[id]; !found {
				return fmt.Errorf("planstore: dispatch without task %s", id)
			}
			if dispatch.ExecutionID != executionID {
				return fmt.Errorf("planstore: immutable dispatch execution identity")
			}
			switch dispatch.State {
			case "pending", "assigned", "in_progress", "pr_open", "gated", "landed":
			default:
				return fmt.Errorf("planstore: invalid operational dispatch state %q", dispatch.State)
			}
		}
		for _, physical := range raw {
			id, _ := physical.Data["task_id"].(string)
			row := known[id]
			row.Dispatch = nil
			if dispatch, found := next[id]; found {
				row.Dispatch = &dispatch
			}
			body, err := wardendb.LosslessDocument(row)
			if err != nil {
				return err
			}
			body["_key"] = row.ID
			if err := d.tasksTx.Update(tx, physical.ID, body, physical.Rev); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}
