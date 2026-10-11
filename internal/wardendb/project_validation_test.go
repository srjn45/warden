package wardendb

import (
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/dbtx"
)

func validRelationships() RelationshipState {
	return RelationshipState{
		Projects: map[string]ProjectRecord{"p": {ID: "p", Status: "open"}},
		Agents: map[string]AgentRecord{
			"root":  {ID: "root", ProjectID: "p", Lifecycle: "stopped"},
			"child": {ID: "child", ProjectID: "p", ParentAgentID: "root", Lifecycle: "stopped"},
		},
		Terminals: map[string]TerminalRecord{"t": {ID: "t", ProjectID: "p"}},
		Plans:     map[string]PlanRecord{"plan": {ID: "plan", ProjectID: "p"}},
		Pipelines: map[string]PipelineRecord{"pipe": {ID: "pipe", ProjectID: "p", PlanID: "plan", ParentAgentID: "root"}},
		Jobs:      map[string]PipelineJobRecord{"pipe:1": {ID: "pipe:1", PipelineID: "pipe", AgentID: "child"}},
		Runs:      map[string]AutopilotRunRecord{"run": {ID: "run", ProjectID: "p", PlanID: "plan", ParentAgentID: "root"}},
	}
}

func TestValidateRelationshipsRequiresTransaction(t *testing.T) {
	if err := ValidateRelationships(nil, validRelationships()); !errors.Is(err, ErrValidationTransaction) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateRelationshipsRejectsCrossProjectAndCycles(t *testing.T) {
	tx := &dbtx.Tx{}
	s := validRelationships()
	s.Projects["other"] = ProjectRecord{ID: "other", Status: "open"}
	a := s.Agents["child"]
	a.ProjectID = "other"
	s.Agents["child"] = a
	if err := ValidateRelationships(tx, s); !errors.Is(err, ErrReference) {
		t.Fatalf("cross project: %v", err)
	}

	s = validRelationships()
	a = s.Agents["root"]
	a.ParentAgentID = "child"
	s.Agents["root"] = a
	if err := ValidateRelationships(tx, s); !errors.Is(err, ErrAgentCycle) {
		t.Fatalf("cycle: %v", err)
	}
}

func TestValidateRelationshipsArchiveAndExplicitGlobalExceptions(t *testing.T) {
	tx := &dbtx.Tx{}
	s := validRelationships()
	s.Agents["global"] = AgentRecord{ID: "global", Scope: ScopeGlobal, Lifecycle: "stopped"}
	if err := ValidateRelationships(tx, s); err != nil {
		t.Fatal(err)
	}
	s.Agents["bad"] = AgentRecord{ID: "bad", Lifecycle: "stopped"}
	if err := ValidateRelationships(tx, s); !errors.Is(err, ErrReference) {
		t.Fatalf("implicit global exception: %v", err)
	}
	delete(s.Agents, "bad")
	a := s.Agents["child"]
	a.Lifecycle, a.ArchivedAt, a.ArchivedFrom = "archived", "2026-10-11T00:00:00Z", "stopped"
	s.Agents["child"] = a
	if err := ValidateRelationships(tx, s); err != nil {
		t.Fatal(err)
	}
	a.ArchivedAt = ""
	s.Agents["child"] = a
	if err := ValidateRelationships(tx, s); !errors.Is(err, ErrArchivedAgent) {
		t.Fatalf("archive state: %v", err)
	}
}

func TestProjectCloseAndDeleteUseCanonicalChildren(t *testing.T) {
	tx := &dbtx.Tx{}
	s := validRelationships()
	if err := ValidateProjectOpen(tx, s, "p"); err != nil {
		t.Fatal(err)
	}
	p := s.Projects["p"]
	p.Status = "closed"
	s.Projects["p"] = p
	if err := ValidateProjectOpen(tx, s, "p"); !errors.Is(err, ErrProjectClosed) {
		t.Fatalf("open gate: %v", err)
	}
	p.Status = "open"
	s.Projects["p"] = p
	if err := ValidateProjectClose(tx, s, "p"); !errors.Is(err, ErrProjectClosed) {
		t.Fatalf("close live project: %v", err)
	}
	for id, a := range s.Agents {
		a.Lifecycle, a.ArchivedAt, a.ArchivedFrom = "archived", "2026-10-11T00:00:00Z", "stopped"
		s.Agents[id] = a
	}
	for id, p := range s.Pipelines {
		p.Lifecycle = "done"
		s.Pipelines[id] = p
	}
	for id, r := range s.Runs {
		r.Lifecycle = "finished"
		s.Runs[id] = r
	}
	if err := ValidateProjectClose(tx, s, "p"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProjectDelete(tx, s, "p"); !errors.Is(err, ErrReferenced) {
		t.Fatalf("delete retained project: %v", err)
	}
}
