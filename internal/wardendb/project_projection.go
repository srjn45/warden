package wardendb

// Project views are disposable read models.  This file deliberately accepts a
// RelationshipState rather than a DB handle: callers have already assembled
// the authoritative candidate state in their write transaction, and the
// projector cannot accidentally make a lifecycle or membership decision from
// a previous projection.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/srjn45/warden/internal/dbtx"
)

var ErrProjectionTransaction = errors.New("wardendb: project projection update requires a transaction")

// ProjectCounters are derived counts, grouped by the child lifecycle. They
// are convenience data only and are never used to validate a write.
type ProjectCounters struct {
	Agents    map[string]int `json:"agents"`
	Terminals map[string]int `json:"terminals"`
	Plans     map[string]int `json:"plans"`
	Pipelines map[string]int `json:"pipelines"`
	Jobs      map[string]int `json:"pipeline_jobs"`
	Runs      map[string]int `json:"autopilot_runs"`
}

// ProjectView is a materialized query result. The contained slices are sorted
// by ID so rebuilds from identical canonical state are byte-stable.
type ProjectView struct {
	Project   ProjectRecord        `json:"project"`
	Agents    []AgentRecord        `json:"agents"`
	Terminals []TerminalRecord     `json:"terminals"`
	Plans     []PlanRecord         `json:"plans"`
	Pipelines []PipelineRecord     `json:"pipelines"`
	Jobs      []PipelineJobRecord  `json:"pipeline_jobs"`
	Runs      []AutopilotRunRecord `json:"autopilot_runs"`
	Counters  ProjectCounters      `json:"counters"`
	Revision  string               `json:"revision"`
}

// ProjectViewProjection is an in-memory derived cache. Its map must never be
// persisted as membership authority.
type ProjectViewProjection struct{ views map[string]ProjectView }

func NewProjectViewProjection() *ProjectViewProjection {
	return &ProjectViewProjection{views: make(map[string]ProjectView)}
}

// Apply rebuilds candidate views before replacing the cache. Thus failed
// rebuilds cannot leave a partially updated projection. tx is deliberately a
// required witness that this update accompanies the canonical write.
func (p *ProjectViewProjection) Apply(tx *dbtx.Tx, state RelationshipState) error {
	if tx == nil {
		return ErrProjectionTransaction
	}
	if p == nil {
		return errors.New("wardendb: nil project projection")
	}
	views, err := RebuildProjectViews(state)
	if err != nil {
		return err
	}
	p.views = views
	return nil
}

// View returns a defensive copy of one cached view.
func (p *ProjectViewProjection) View(projectID string) (ProjectView, bool) {
	if p == nil {
		return ProjectView{}, false
	}
	v, ok := p.views[projectID]
	return cloneProjectView(v), ok
}

// RebuildProjectViews derives every view from canonical child-to-parent
// references. It intentionally does not call validation: repair tooling uses
// it to inspect malformed candidate states as well as valid ones.
func RebuildProjectViews(s RelationshipState) (map[string]ProjectView, error) {
	views := make(map[string]ProjectView, len(s.Projects))
	for id, project := range s.Projects {
		if id == "" || project.ID != id {
			return nil, fmt.Errorf("wardendb: invalid project %q for projection", id)
		}
		views[id] = ProjectView{Project: project, Counters: newProjectCounters()}
	}
	for _, a := range s.Agents {
		addAgent(views, a)
	}
	for _, t := range s.Terminals {
		addTerminal(views, t)
	}
	for _, plan := range s.Plans {
		addPlan(views, plan)
	}
	for _, pipeline := range s.Pipelines {
		addPipeline(views, pipeline)
	}
	// Jobs inherit ownership only through their canonical pipeline FK.
	for _, job := range s.Jobs {
		if pipeline, ok := s.Pipelines[job.PipelineID]; ok {
			addJob(views, pipeline.ProjectID, job)
		}
	}
	for _, run := range s.Runs {
		addRun(views, run)
	}
	for id, view := range views {
		sortProjectView(&view)
		revision, err := projectViewRevision(view)
		if err != nil {
			return nil, err
		}
		view.Revision = revision
		views[id] = view
	}
	return views, nil
}

// CompareProjectViews rebuilds from canonical collections and reports every
// cache entry which is absent, stale, or unexpectedly present.
func CompareProjectViews(actual map[string]ProjectView, canonical RelationshipState) error {
	want, err := RebuildProjectViews(canonical)
	if err != nil {
		return err
	}
	for id, expected := range want {
		got, ok := actual[id]
		if !ok {
			return fmt.Errorf("wardendb: project view %q missing", id)
		}
		if !reflect.DeepEqual(cloneProjectView(got), expected) {
			return fmt.Errorf("wardendb: project view %q differs from canonical rebuild", id)
		}
	}
	for id := range actual {
		if _, ok := want[id]; !ok {
			return fmt.Errorf("wardendb: unexpected project view %q", id)
		}
	}
	return nil
}

func newProjectCounters() ProjectCounters {
	return ProjectCounters{map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}}
}
func addAgent(v map[string]ProjectView, r AgentRecord) {
	if x, ok := v[r.ProjectID]; ok {
		x.Agents = append(x.Agents, r)
		x.Counters.Agents[r.Lifecycle]++
		v[r.ProjectID] = x
	}
}
func addTerminal(v map[string]ProjectView, r TerminalRecord) {
	if x, ok := v[r.ProjectID]; ok {
		x.Terminals = append(x.Terminals, r)
		x.Counters.Terminals[r.Lifecycle]++
		v[r.ProjectID] = x
	}
}
func addPlan(v map[string]ProjectView, r PlanRecord) {
	if x, ok := v[r.ProjectID]; ok {
		x.Plans = append(x.Plans, r)
		x.Counters.Plans[r.Lifecycle]++
		v[r.ProjectID] = x
	}
}
func addPipeline(v map[string]ProjectView, r PipelineRecord) {
	if x, ok := v[r.ProjectID]; ok {
		x.Pipelines = append(x.Pipelines, r)
		x.Counters.Pipelines[r.Lifecycle]++
		v[r.ProjectID] = x
	}
}
func addJob(v map[string]ProjectView, projectID string, r PipelineJobRecord) {
	if x, ok := v[projectID]; ok {
		x.Jobs = append(x.Jobs, r)
		x.Counters.Jobs[r.Lifecycle]++
		v[projectID] = x
	}
}
func addRun(v map[string]ProjectView, r AutopilotRunRecord) {
	if x, ok := v[r.ProjectID]; ok {
		x.Runs = append(x.Runs, r)
		x.Counters.Runs[r.Lifecycle]++
		v[r.ProjectID] = x
	}
}
func sortProjectView(v *ProjectView) {
	sort.Slice(v.Agents, func(i, j int) bool { return v.Agents[i].ID < v.Agents[j].ID })
	sort.Slice(v.Terminals, func(i, j int) bool { return v.Terminals[i].ID < v.Terminals[j].ID })
	sort.Slice(v.Plans, func(i, j int) bool { return v.Plans[i].ID < v.Plans[j].ID })
	sort.Slice(v.Pipelines, func(i, j int) bool { return v.Pipelines[i].ID < v.Pipelines[j].ID })
	sort.Slice(v.Jobs, func(i, j int) bool { return v.Jobs[i].ID < v.Jobs[j].ID })
	sort.Slice(v.Runs, func(i, j int) bool { return v.Runs[i].ID < v.Runs[j].ID })
}
func projectViewRevision(v ProjectView) (string, error) {
	v.Revision = ""
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "pv:" + hex.EncodeToString(sum[:]), nil
}
func cloneProjectView(v ProjectView) ProjectView {
	b, err := CanonicalJSON(v)
	if err != nil {
		return ProjectView{}
	}
	var out ProjectView
	if json.Unmarshal(b, &out) != nil {
		return ProjectView{}
	}
	return out
}
