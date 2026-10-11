package workflows

import (
	"github.com/srjn45/scriva"
	"github.com/srjn45/warden/internal/dbtx"
)

// The Check… functions are pure reads. Each holds for ANY database state: it
// reports the cross-collection invariant of one workflow, so a crash/fault suite
// can call it after every injected failure and reopen.

func load[T any](db *scriva.DB, name string) ([]dbtx.Versioned[T], error) {
	r, err := newReader[T](db, name)
	if err != nil {
		return nil, err
	}
	return r.All()
}

func byID[T any](rows []dbtx.Versioned[T]) map[string]T {
	m := make(map[string]T, len(rows))
	for _, r := range rows {
		m[keyOf(r.Value)] = r.Value
	}
	return m
}

// dupKeys reports logical ids that appear more than once in one collection.
func dupKeys[T any](rows []dbtx.Versioned[T], v *violations, coll string) {
	seen := map[string]bool{}
	for _, r := range rows {
		k := keyOf(r.Value)
		if seen[k] {
			v.addf("%s: duplicate id %q", coll, k)
		}
		seen[k] = true
	}
}

// CheckProjectChildren (workflow 1): every agent belongs to an existing project
// (no orphan child), ids are unique, and each project's derived live_agents
// counter equals its actual live children.
func CheckProjectChildren(db *scriva.DB) error {
	v := &violations{checker: "CheckProjectChildren"}
	projRows, err := load[Project](db, CollProjects)
	if err != nil {
		return err
	}
	agentRows, err := load[Agent](db, CollAgents)
	if err != nil {
		return err
	}
	dupKeys(projRows, v, CollProjects)
	dupKeys(agentRows, v, CollAgents)
	projects := byID(projRows)
	live := map[string]int{}
	for _, a := range agentRows {
		if _, ok := projects[a.Value.ProjectID]; !ok {
			v.addf("agent %q has no project %q", a.Value.ID, a.Value.ProjectID)
			continue
		}
		live[a.Value.ProjectID]++
	}
	for id, p := range projects {
		if p.LiveAgents != live[id] {
			v.addf("project %q live_agents=%d but %d live agents", id, p.LiveAgents, live[id])
		}
	}
	return v.err()
}

// CheckAutopilotStartup (workflow 2): a non-terminal run implies its plan is
// running and points back at it, its manager agent slot exists with the run's
// id, and the launch intent is journaled; conversely a plan/agent pointing at a
// run requires that run, and at most one live run exists per plan.
func CheckAutopilotStartup(db *scriva.DB) error {
	v := &violations{checker: "CheckAutopilotStartup"}
	runRows, err := load[Run](db, CollRuns)
	if err != nil {
		return err
	}
	planRows, err := load[Plan](db, CollPlans)
	if err != nil {
		return err
	}
	agentRows, err := load[Agent](db, CollAgents)
	if err != nil {
		return err
	}
	opRows, err := load[Operation](db, CollOperations)
	if err != nil {
		return err
	}
	dupKeys(runRows, v, CollRuns)
	runs, plans, agents, ops := byID(runRows), byID(planRows), byID(agentRows), byID(opRows)
	livePerPlan := map[string]int{}
	for _, r := range runs {
		if r.Status != "running" {
			continue
		}
		livePerPlan[r.PlanID]++
		pl, ok := plans[r.PlanID]
		switch {
		case !ok:
			v.addf("run %q: plan %q missing", r.ID, r.PlanID)
		case pl.Status != "running" || pl.RunID != r.ID:
			v.addf("run %q: plan %q is %s/run=%q (not flipped to this run)", r.ID, pl.ID, pl.Status, pl.RunID)
		}
		if r.LivePlan != r.PlanID {
			v.addf("run %q: live_plan=%q want %q", r.ID, r.LivePlan, r.PlanID)
		}
		m, ok := agents[r.ManagerAgentID]
		switch {
		case !ok:
			v.addf("run %q: manager slot %q missing", r.ID, r.ManagerAgentID)
		case m.RunID != r.ID || m.Role != "manager" || m.ProjectID != r.ProjectID:
			v.addf("run %q: manager %q mislinked (run=%q role=%q project=%q)", r.ID, m.ID, m.RunID, m.Role, m.ProjectID)
		}
		op, ok := ops[launchOpID(r.ManagerAgentID)]
		switch {
		case !ok:
			v.addf("run %q: launch intent for %q not journaled", r.ID, r.ManagerAgentID)
		case ok && m.Status == "running" && op.State != "succeeded":
			v.addf("run %q: manager running but launch op is %s", r.ID, op.State)
		case ok && op.State == "succeeded" && m.Status == "starting":
			v.addf("run %q: launch op succeeded but manager still starting", r.ID)
		}
	}
	for pid, n := range livePerPlan {
		if n > 1 {
			v.addf("plan %q has %d live runs", pid, n)
		}
	}
	for _, p := range plans {
		if p.RunID == "" {
			continue
		}
		if _, ok := runs[p.RunID]; !ok {
			v.addf("plan %q points at missing run %q", p.ID, p.RunID)
		}
	}
	for _, a := range agents {
		if a.RunID == "" {
			continue
		}
		if _, ok := runs[a.RunID]; !ok {
			v.addf("agent %q points at missing run %q", a.ID, a.RunID)
		}
	}
	return v.err()
}

// CheckPipelineLinkage (workflow 3): the plan⇄pipeline link is present on both
// sides or neither, each pipeline is claimed by one plan, and a pipeline's job
// rows match its derived job count.
func CheckPipelineLinkage(db *scriva.DB) error {
	v := &violations{checker: "CheckPipelineLinkage"}
	planRows, err := load[Plan](db, CollPlans)
	if err != nil {
		return err
	}
	pipeRows, err := load[Pipeline](db, CollPipelines)
	if err != nil {
		return err
	}
	jobRows, err := load[PipelineJob](db, CollPipelineJobs)
	if err != nil {
		return err
	}
	dupKeys(pipeRows, v, CollPipelines)
	plans, pipes := byID(planRows), byID(pipeRows)
	for _, pl := range plans {
		if pl.PipelineID == "" {
			continue
		}
		pp, ok := pipes[pl.PipelineID]
		switch {
		case !ok:
			v.addf("plan %q -> pipeline %q missing", pl.ID, pl.PipelineID)
		case pp.PlanID != pl.ID:
			v.addf("plan %q -> pipeline %q but pipeline.plan_id=%q", pl.ID, pp.ID, pp.PlanID)
		}
	}
	claims := map[string]int{}
	for _, pp := range pipes {
		if pp.PlanID == "" {
			continue
		}
		claims[pp.PlanID]++
		pl, ok := plans[pp.PlanID]
		switch {
		case !ok:
			v.addf("pipeline %q -> plan %q missing", pp.ID, pp.PlanID)
		case pl.PipelineID != pp.ID:
			v.addf("pipeline %q -> plan %q but plan.pipeline_id=%q", pp.ID, pl.ID, pl.PipelineID)
		}
	}
	for id, n := range claims {
		if n > 1 {
			v.addf("plan %q claimed by %d pipelines", id, n)
		}
	}
	jobs := map[string]int{}
	for _, j := range jobRows {
		if _, ok := pipes[j.Value.PipelineID]; !ok {
			v.addf("job %q has no pipeline %q", j.Value.ID, j.Value.PipelineID)
			continue
		}
		jobs[j.Value.PipelineID]++
	}
	for id, pp := range pipes {
		if pp.Jobs != jobs[id] {
			v.addf("pipeline %q jobs=%d but %d job rows", id, pp.Jobs, jobs[id])
		}
	}
	return v.err()
}

// CheckArchive (workflow 4): an agent is in exactly one of live/archive, archive
// rows are marked archived with their prior status and a project, live rows are
// not marked archived, and each project's live_agents/archived_agents counters
// equal its live/archive rows (so a live row deleted without its archive row,
// or the reverse, is detected).
func CheckArchive(db *scriva.DB) error {
	v := &violations{checker: "CheckArchive"}
	liveRows, err := load[Agent](db, CollAgents)
	if err != nil {
		return err
	}
	archRows, err := load[Agent](db, CollAgentsArchive)
	if err != nil {
		return err
	}
	projRows, err := load[Project](db, CollProjects)
	if err != nil {
		return err
	}
	dupKeys(archRows, v, CollAgentsArchive)
	live, projects := byID(liveRows), byID(projRows)
	archived := map[string]int{}
	for _, a := range archRows {
		x := a.Value
		if _, both := live[x.ID]; both {
			v.addf("agent %q is in both live and archive", x.ID)
		}
		if x.Status != "archived" || x.ArchivedFrom == "" {
			v.addf("archived agent %q has status=%q archived_from=%q", x.ID, x.Status, x.ArchivedFrom)
		}
		if _, ok := projects[x.ProjectID]; !ok {
			v.addf("archived agent %q has no project %q", x.ID, x.ProjectID)
		}
		archived[x.ProjectID]++
	}
	liveBy := map[string]int{}
	for _, a := range live {
		liveBy[a.ProjectID]++
		if a.Status == "archived" || a.ArchivedFrom != "" {
			v.addf("live agent %q carries archive markers (%s/%s)", a.ID, a.Status, a.ArchivedFrom)
		}
	}
	for id, p := range projects {
		if p.LiveAgents != liveBy[id] {
			v.addf("project %q live_agents=%d but %d live rows (agent lost or duplicated by a move)", id, p.LiveAgents, liveBy[id])
		}
		if p.ArchivedAgents != archived[id] {
			v.addf("project %q archived_agents=%d but %d archive rows", id, p.ArchivedAgents, archived[id])
		}
	}
	return v.err()
}

// CheckProjectClosure (workflow 5): a closed project has no open agent, no
// running pipeline and no live autopilot run.
func CheckProjectClosure(db *scriva.DB) error {
	v := &violations{checker: "CheckProjectClosure"}
	projRows, err := load[Project](db, CollProjects)
	if err != nil {
		return err
	}
	agentRows, err := load[Agent](db, CollAgents)
	if err != nil {
		return err
	}
	pipeRows, err := load[Pipeline](db, CollPipelines)
	if err != nil {
		return err
	}
	runRows, err := load[Run](db, CollRuns)
	if err != nil {
		return err
	}
	closed := map[string]bool{}
	for _, p := range projRows {
		switch p.Value.Status {
		case "closed":
			closed[p.Value.ID] = true
		case "open":
		default:
			v.addf("project %q has unknown status %q", p.Value.ID, p.Value.Status)
		}
	}
	for _, a := range agentRows {
		if closed[a.Value.ProjectID] && isOpen(a.Value.Status) {
			v.addf("closed project %q has %s agent %q", a.Value.ProjectID, a.Value.Status, a.Value.ID)
		}
	}
	for _, p := range pipeRows {
		if closed[p.Value.ProjectID] && p.Value.Status == "running" {
			v.addf("closed project %q has running pipeline %q", p.Value.ProjectID, p.Value.ID)
		}
	}
	for _, r := range runRows {
		if closed[r.Value.ProjectID] && r.Value.Status == "running" {
			v.addf("closed project %q has live run %q", r.Value.ProjectID, r.Value.ID)
		}
	}
	return v.err()
}

// CheckProjection (workflow 6): every persisted view's project exists and the
// view's source_version and copied fields equal the source's — no stale view.
func CheckProjection(db *scriva.DB) error {
	v := &violations{checker: "CheckProjection"}
	projRows, err := load[Project](db, CollProjects)
	if err != nil {
		return err
	}
	viewRows, err := load[View](db, CollViews)
	if err != nil {
		return err
	}
	dupKeys(viewRows, v, CollViews)
	projects := byID(projRows)
	for _, vw := range viewRows {
		p, ok := projects[vw.Value.ID]
		if !ok {
			v.addf("view %q has no project", vw.Value.ID)
			continue
		}
		if want := viewOf(p); vw.Value != want {
			v.addf("view %q is %+v, source says %+v", vw.Value.ID, vw.Value, want)
		}
	}
	return v.err()
}

// CheckAuditTrail (workflow 7): a plan's audit events are exactly one per
// revision 1..Revision (none missing, none beyond), carry the right id, and the
// latest event's status equals the plan's status.
func CheckAuditTrail(db *scriva.DB) error {
	v := &violations{checker: "CheckAuditTrail"}
	planRows, err := load[Plan](db, CollPlans)
	if err != nil {
		return err
	}
	evRows, err := load[Event](db, CollEvents)
	if err != nil {
		return err
	}
	dupKeys(evRows, v, CollEvents)
	plans := byID(planRows)
	byPlan := map[string]map[uint64]Event{}
	for _, r := range evRows {
		e := r.Value
		if e.ID != eventID(e.Subject, e.SubjectRev) {
			v.addf("event %q has subject=%q rev=%d", e.ID, e.Subject, e.SubjectRev)
		}
		if _, ok := plans[e.Subject]; !ok {
			v.addf("event %q describes missing plan %q", e.ID, e.Subject)
			continue
		}
		if byPlan[e.Subject] == nil {
			byPlan[e.Subject] = map[uint64]Event{}
		}
		byPlan[e.Subject][e.SubjectRev] = e
	}
	for id, p := range plans {
		evs := byPlan[id]
		for rev := uint64(1); rev <= p.Revision; rev++ {
			if _, ok := evs[rev]; !ok {
				v.addf("plan %q revision %d has no event", id, rev)
			}
		}
		for rev := range evs {
			if rev > p.Revision || rev == 0 {
				v.addf("plan %q has event for revision %d beyond plan revision %d", id, rev, p.Revision)
			}
		}
		if last, ok := evs[p.Revision]; ok && last.To != p.Status {
			v.addf("plan %q status %q but latest event says %q", id, p.Status, last.To)
		}
	}
	return v.err()
}
