package wardendb

// The relationship validator deliberately works on a caller supplied candidate
// state. Writers construct that state from their transaction's intended writes,
// validate it, then stage exactly those writes on the same dbtx.Tx. This makes
// a relationship decision independent of ProjectView and other derived data.

import (
	"errors"
	"fmt"

	"github.com/srjn45/warden/internal/dbtx"
)

var (
	ErrValidationTransaction = errors.New("wardendb: relationship validation requires a transaction")
	ErrReference             = errors.New("wardendb: invalid reference")
	ErrProjectClosed         = errors.New("wardendb: project is closed")
	ErrAgentCycle            = errors.New("wardendb: agent ancestry cycle")
	ErrArchivedAgent         = errors.New("wardendb: invalid archived agent state")
	ErrReferenced            = errors.New("wardendb: record is still referenced")
)

// ScopeGlobal and ScopeSystem are the only explicit exceptions to project
// ownership. They are intentionally record-level declarations, rather than a
// permissive missing-project fallback.
const (
	ScopeGlobal = "global"
	ScopeSystem = "system"
)

// RelationshipState contains authoritative records only. ProjectView, counters
// and any cached child lists are intentionally absent.
type RelationshipState struct {
	Projects  map[string]ProjectRecord
	Agents    map[string]AgentRecord
	Terminals map[string]TerminalRecord
	Plans     map[string]PlanRecord
	Pipelines map[string]PipelineRecord
	Jobs      map[string]PipelineJobRecord
	Runs      map[string]AutopilotRunRecord
}

// ValidateRelationships checks a complete candidate state. It may only be
// called while the caller is staging the corresponding dbtx transaction.
func ValidateRelationships(tx *dbtx.Tx, s RelationshipState) error {
	if tx == nil {
		return ErrValidationTransaction
	}
	for id, p := range s.Projects {
		if id == "" || p.ID != id || (p.Status != "open" && p.Status != "closed") {
			return fmt.Errorf("%w: project %q", ErrReference, id)
		}
	}
	for id, a := range s.Agents {
		if id == "" || a.ID != id {
			return fmt.Errorf("%w: agent %q", ErrReference, id)
		}
		if a.Scope != "" && a.Scope != ScopeGlobal && a.Scope != ScopeSystem {
			return fmt.Errorf("%w: agent %q has unknown scope %q", ErrReference, id, a.Scope)
		}
		if a.ProjectID == "" {
			if a.Scope != ScopeGlobal && a.Scope != ScopeSystem {
				return fmt.Errorf("%w: agent %q has no project (scope must be global or system)", ErrReference, id)
			}
		} else if _, ok := s.Projects[a.ProjectID]; !ok {
			return missing("agent", id, "project", a.ProjectID)
		}
		if a.ParentAgentID != "" {
			parent, ok := s.Agents[a.ParentAgentID]
			if !ok {
				return missing("agent", id, "parent agent", a.ParentAgentID)
			}
			if parent.ProjectID != a.ProjectID {
				return crossProject("agent", id, "parent agent", a.ParentAgentID)
			}
		}
		if err := agentArchiveState(a); err != nil {
			return fmt.Errorf("%w: agent %q", err, id)
		}
		if err := requireSameProject(s, "agent", id, a.ProjectID, "plan", a.PlanID); err != nil {
			return err
		}
		if err := requireSameProject(s, "agent", id, a.ProjectID, "pipeline", a.PipelineID); err != nil {
			return err
		}
	}
	if err := validateAgentForest(s.Agents); err != nil {
		return err
	}
	for id, t := range s.Terminals {
		if id == "" || t.ID != id {
			return fmt.Errorf("%w: terminal %q", ErrReference, id)
		}
		if err := requireProject(s, "terminal", id, t.ProjectID); err != nil {
			return err
		}
	}
	for id, p := range s.Plans {
		if id == "" || p.ID != id {
			return fmt.Errorf("%w: plan %q", ErrReference, id)
		}
		if err := requireProject(s, "plan", id, p.ProjectID); err != nil {
			return err
		}
		if p.PipelineID != "" {
			if err := requireSameProject(s, "plan", id, p.ProjectID, "pipeline", p.PipelineID); err != nil {
				return err
			}
		}
	}
	for id, p := range s.Pipelines {
		if id == "" || p.ID != id {
			return fmt.Errorf("%w: pipeline %q", ErrReference, id)
		}
		if err := requireProject(s, "pipeline", id, p.ProjectID); err != nil {
			return err
		}
		if err := requireSameProject(s, "pipeline", id, p.ProjectID, "plan", p.PlanID); err != nil {
			return err
		}
		if p.ParentAgentID != "" {
			if err := requireSameProject(s, "pipeline", id, p.ProjectID, "agent", p.ParentAgentID); err != nil {
				return err
			}
		}
	}
	for id, j := range s.Jobs {
		if id == "" || j.ID != id {
			return fmt.Errorf("%w: pipeline job %q", ErrReference, id)
		}
		pipeline, ok := s.Pipelines[j.PipelineID]
		if !ok {
			return missing("pipeline job", id, "pipeline", j.PipelineID)
		}
		if j.AgentID != "" {
			if err := requireSameProject(s, "pipeline job", id, pipeline.ProjectID, "agent", j.AgentID); err != nil {
				return err
			}
		}
	}
	for id, r := range s.Runs {
		if id == "" || r.ID != id {
			return fmt.Errorf("%w: autopilot run %q", ErrReference, id)
		}
		if err := requireProject(s, "autopilot run", id, r.ProjectID); err != nil {
			return err
		}
		if err := requireSameProject(s, "autopilot run", id, r.ProjectID, "plan", r.PlanID); err != nil {
			return err
		}
		if r.ParentAgentID != "" {
			if err := requireSameProject(s, "autopilot run", id, r.ProjectID, "agent", r.ParentAgentID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateProjectOpen is the lifecycle gate for any create or active-state
// change. Existing children remain valid after a project is closed, but a
// writer cannot attach new work until it has been explicitly reopened.
func ValidateProjectOpen(tx *dbtx.Tx, s RelationshipState, projectID string) error {
	if tx == nil {
		return ErrValidationTransaction
	}
	p, ok := s.Projects[projectID]
	if !ok {
		return missing("project", projectID, "project", projectID)
	}
	if p.Status != "open" {
		return fmt.Errorf("%w: %q", ErrProjectClosed, projectID)
	}
	return nil
}

// ValidateProjectClose ensures a close operation cannot strand live work. The
// caller stages the project status update in the same transaction after this
// check succeeds. Archived agents are retained references and are allowed.
func ValidateProjectClose(tx *dbtx.Tx, s RelationshipState, projectID string) error {
	if tx == nil {
		return ErrValidationTransaction
	}
	if _, ok := s.Projects[projectID]; !ok {
		return missing("project", projectID, "project", projectID)
	}
	for id, a := range s.Agents {
		if a.ProjectID == projectID && a.Lifecycle != "archived" {
			return fmt.Errorf("%w: project %q has live agent %q", ErrProjectClosed, projectID, id)
		}
	}
	for id, p := range s.Pipelines {
		if p.ProjectID == projectID && !terminalLifecycle(p.Lifecycle) {
			return fmt.Errorf("%w: project %q has live pipeline %q", ErrProjectClosed, projectID, id)
		}
	}
	for id, r := range s.Runs {
		if r.ProjectID == projectID && !terminalLifecycle(r.Lifecycle) {
			return fmt.Errorf("%w: project %q has live run %q", ErrProjectClosed, projectID, id)
		}
	}
	return nil
}

// ValidateProjectDelete applies restricted-parent-delete semantics. It is
// separate from close: a closed project remains authoritative and may have
// retained archived children.
func ValidateProjectDelete(tx *dbtx.Tx, s RelationshipState, projectID string) error {
	if tx == nil {
		return ErrValidationTransaction
	}
	for _, a := range s.Agents {
		if a.ProjectID == projectID {
			return fmt.Errorf("%w: project %q is referenced by agents", ErrReferenced, projectID)
		}
	}
	for _, t := range s.Terminals {
		if t.ProjectID == projectID {
			return fmt.Errorf("%w: project %q is referenced by terminals", ErrReferenced, projectID)
		}
	}
	for _, p := range s.Plans {
		if p.ProjectID == projectID {
			return fmt.Errorf("%w: project %q is referenced by plans", ErrReferenced, projectID)
		}
	}
	for _, p := range s.Pipelines {
		if p.ProjectID == projectID {
			return fmt.Errorf("%w: project %q is referenced by pipelines", ErrReferenced, projectID)
		}
	}
	for _, r := range s.Runs {
		if r.ProjectID == projectID {
			return fmt.Errorf("%w: project %q is referenced by runs", ErrReferenced, projectID)
		}
	}
	return nil
}

func agentArchiveState(a AgentRecord) error {
	if a.Lifecycle == "archived" && (a.ArchivedAt == "" || a.ArchivedFrom == "") {
		return ErrArchivedAgent
	}
	if a.Lifecycle != "archived" && (a.ArchivedAt != "" || a.ArchivedFrom != "") {
		return ErrArchivedAgent
	}
	return nil
}
func validateAgentForest(agents map[string]AgentRecord) error {
	for start := range agents {
		seen := map[string]bool{}
		for id := start; id != ""; id = agents[id].ParentAgentID {
			if seen[id] {
				return fmt.Errorf("%w: %q", ErrAgentCycle, id)
			}
			seen[id] = true
		}
	}
	return nil
}
func requireProject(s RelationshipState, child, id, projectID string) error {
	if projectID == "" {
		return fmt.Errorf("%w: %s %q has no project", ErrReference, child, id)
	}
	if _, ok := s.Projects[projectID]; !ok {
		return missing(child, id, "project", projectID)
	}
	return nil
}
func requireSameProject(s RelationshipState, child, id, projectID, parentKind, parentID string) error {
	if parentID == "" {
		return nil
	}
	var parentProject string
	switch parentKind {
	case "agent":
		a, ok := s.Agents[parentID]
		if !ok {
			return missing(child, id, parentKind, parentID)
		}
		parentProject = a.ProjectID
	case "plan":
		p, ok := s.Plans[parentID]
		if !ok {
			return missing(child, id, parentKind, parentID)
		}
		parentProject = p.ProjectID
	case "pipeline":
		p, ok := s.Pipelines[parentID]
		if !ok {
			return missing(child, id, parentKind, parentID)
		}
		parentProject = p.ProjectID
	default:
		return fmt.Errorf("%w: unsupported parent kind %q", ErrReference, parentKind)
	}
	if projectID == "" || parentProject != projectID {
		return crossProject(child, id, parentKind, parentID)
	}
	return nil
}
func missing(child, id, parent, parentID string) error {
	return fmt.Errorf("%w: %s %q references missing %s %q", ErrReference, child, id, parent, parentID)
}
func crossProject(child, id, parent, parentID string) error {
	return fmt.Errorf("%w: %s %q and %s %q have different projects", ErrReference, child, id, parent, parentID)
}
func terminalLifecycle(s string) bool {
	return s == "done" || s == "failed" || s == "canceled" || s == "finished" || s == "stopped" || s == "archived"
}
