package workflows

import (
	"fmt"

	"github.com/srjn45/warden/internal/dbtx"
)

// openProject reads a project that must exist and be open.
func (e *Env) openProject(id string) (dbtx.Versioned[Project], error) {
	p, ok, err := e.Projects.ByKey(id)
	if err != nil {
		return p, err
	}
	if !ok {
		return p, fmt.Errorf("%w: %s", ErrProjectMissing, id)
	}
	if p.Value.Status != "open" {
		return p, fmt.Errorf("%w: %s", ErrProjectClosed, id)
	}
	return p, nil
}

func viewOf(p Project) View {
	return View{ID: p.ID, Name: p.Name, Status: p.Status, LiveAgents: p.LiveAgents,
		ArchivedAgents: p.ArchivedAgents, SourceVersion: p.Version}
}

// stageProject stages a fenced (ExpectedRev) project update that bumps Version
// and, in the same transaction, refreshes the project's persisted view — so a
// view can never be observed stale relative to its source. ensureView also
// creates a missing view.
func (e *Env) stageProject(tx *dbtx.Tx, cur dbtx.Versioned[Project], mut func(*Project), ensureView bool) error {
	next := cur.Value
	mut(&next)
	next.Version = cur.Value.Version + 1
	if err := e.Projects.Update(tx, cur.ID, next, cur.Rev); err != nil {
		return err
	}
	v, ok, err := e.Views.ByKey(next.ID)
	if err != nil {
		return err
	}
	switch {
	case ok:
		return e.Views.Update(tx, v.ID, viewOf(next), v.Rev)
	case ensureView:
		_, err = e.Views.Insert(tx, viewOf(next))
		return err
	}
	return nil
}

// fenceProject stages an identity update of the project solely so a concurrent
// writer of that project (e.g. a close) makes this commit conflict: a validation
// read is only trustworthy when fenced with ExpectedRev (audit G1).
func (e *Env) fenceProject(tx *dbtx.Tx, cur dbtx.Versioned[Project]) error {
	return e.Projects.Update(tx, cur.ID, cur.Value, cur.Rev)
}

// stageAgent inserts a live agent and bumps the project's live counter.
func (e *Env) stageAgent(tx *dbtx.Tx, proj dbtx.Versioned[Project], a Agent) error {
	if _, err := e.Agents.Insert(tx, a); err != nil {
		return err
	}
	return e.stageProject(tx, proj, func(p *Project) { p.LiveAgents++ }, false)
}

func eventID(plan string, rev uint64) string { return fmt.Sprintf("%s@%d", plan, rev) }

// stagePlan stages a fenced plan update that bumps Revision and appends the
// matching audit event in the same transaction.
func (e *Env) stagePlan(tx *dbtx.Tx, cur dbtx.Versioned[Plan], kind string, mut func(*Plan)) error {
	next := cur.Value
	mut(&next)
	next.Revision = cur.Value.Revision + 1
	if err := e.Plans.Update(tx, cur.ID, next, cur.Rev); err != nil {
		return err
	}
	_, err := e.Events.Insert(tx, Event{ID: eventID(next.ID, next.Revision), Kind: kind,
		Subject: next.ID, SubjectRev: next.Revision, To: next.Status})
	return err
}

func intent(workflow, id string) string { return "wf/" + workflow + "/" + id }

// isRunning reports whether an agent/pipeline/run status counts as open work.
func isOpen(status string) bool { return status == "starting" || status == "running" }
