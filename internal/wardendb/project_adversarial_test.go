package wardendb

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/srjn45/warden/internal/dbtx"
)

func TestProjectRelationshipsRejectAdversarialReferences(t *testing.T) {
	for name, mutate := range map[string]func(*RelationshipState){
		"terminal missing project": func(s *RelationshipState) {
			s.Terminals["t"] = TerminalRecord{ID: "t", ProjectID: "gone"}
		},
		"agent cross-project plan": func(s *RelationshipState) {
			s.Projects["other"] = ProjectRecord{ID: "other", Status: "open"}
			a := s.Agents["child"]
			a.PlanID = "plan"
			a.ProjectID = "other"
			s.Agents["child"] = a
		},
		"pipeline job cross-project agent": func(s *RelationshipState) {
			s.Projects["other"] = ProjectRecord{ID: "other", Status: "open"}
			s.Agents["other-agent"] = AgentRecord{ID: "other-agent", ProjectID: "other", Lifecycle: "stopped"}
			j := s.Jobs["pipe:1"]
			j.AgentID = "other-agent"
			s.Jobs["pipe:1"] = j
		},
		"run missing plan": func(s *RelationshipState) {
			r := s.Runs["run"]
			r.PlanID = "missing"
			s.Runs["run"] = r
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := validRelationships()
			mutate(&s)
			if err := ValidateRelationships(&dbtx.Tx{}, s); !errors.Is(err, ErrReference) {
				t.Fatalf("ValidateRelationships() error = %v, want ErrReference", err)
			}
		})
	}
}

func TestProjectViewConcurrentApplyNeverExposesPartialRebuild(t *testing.T) {
	p := NewProjectViewProjection()
	initial := projectionState()
	if err := p.Apply(&dbtx.Tx{}, initial); err != nil {
		t.Fatal(err)
	}
	updated := projectionState()
	updated.Agents["a3"] = AgentRecord{ID: "a3", ProjectID: "p1", Lifecycle: "busy"}

	var wg sync.WaitGroup
	errCh := make(chan error, 200)
	for i := 0; i < 100; i++ {
		state := initial
		if i%2 == 1 {
			state = updated
		}
		wg.Add(2)
		go func(state RelationshipState) {
			defer wg.Done()
			if err := p.Apply(&dbtx.Tx{}, state); err != nil {
				errCh <- err
			}
		}(state)
		go func() {
			defer wg.Done()
			view, ok := p.View("p1")
			if !ok || (len(view.Agents) != 2 && len(view.Agents) != 3) {
				errCh <- fmt.Errorf("observed partial view: %#v", view)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestProjectViewFailedRebuildPreservesPreviousCacheAndRecovers(t *testing.T) {
	p := NewProjectViewProjection()
	good := projectionState()
	if err := p.Apply(&dbtx.Tx{}, good); err != nil {
		t.Fatal(err)
	}
	before, ok := p.View("p1")
	if !ok {
		t.Fatal("missing initial projection")
	}

	broken := projectionState()
	broken.Projects["p1"] = ProjectRecord{ID: "wrong-id", Status: "open"}
	if err := p.Apply(&dbtx.Tx{}, broken); err == nil {
		t.Fatal("Apply accepted malformed canonical state")
	}
	after, ok := p.View("p1")
	if !ok || after.Revision != before.Revision || len(after.Agents) != len(before.Agents) {
		t.Fatalf("failed rebuild replaced cache: before=%#v after=%#v", before, after)
	}

	repaired := projectionState()
	repaired.Agents["recovered"] = AgentRecord{ID: "recovered", ProjectID: "p1", Lifecycle: "idle"}
	if err := p.Apply(&dbtx.Tx{}, repaired); err != nil {
		t.Fatalf("Apply repaired state: %v", err)
	}
	actual := map[string]ProjectView{}
	for _, id := range []string{"p1", "p2"} {
		actual[id], _ = p.View(id)
	}
	if err := CompareProjectViews(actual, repaired); err != nil {
		t.Fatalf("recovered cache differs from canonical rebuild: %v", err)
	}
}

func TestProjectViewArchiveRestoreRoundTripMatchesCanonicalRebuild(t *testing.T) {
	p := NewProjectViewProjection()
	original := projectionState()
	delete(original.Agents, "outside")
	delete(original.Jobs, "orphan")
	if err := p.Apply(&dbtx.Tx{}, original); err != nil {
		t.Fatal(err)
	}
	before, _ := p.View("p1")

	archived := projectionState()
	delete(archived.Agents, "outside")
	delete(archived.Jobs, "orphan")
	a := archived.Agents["a1"]
	a.Lifecycle, a.ArchivedAt, a.ArchivedFrom = "archived", "2026-10-11T00:00:00Z", "idle"
	archived.Agents["a1"] = a
	if err := ValidateRelationships(&dbtx.Tx{}, archived); err != nil {
		t.Fatalf("archive state invalid: %v", err)
	}
	if err := p.Apply(&dbtx.Tx{}, archived); err != nil {
		t.Fatal(err)
	}
	archivedView, _ := p.View("p1")
	if archivedView.Counters.Agents["archived"] != 1 {
		t.Fatalf("archive was not reflected in derived counters: %#v", archivedView.Counters)
	}

	if err := p.Apply(&dbtx.Tx{}, original); err != nil {
		t.Fatalf("restore projection: %v", err)
	}
	restored, _ := p.View("p1")
	if restored.Revision != before.Revision {
		t.Fatalf("archive/restore changed canonical-equivalent revision: %q != %q", restored.Revision, before.Revision)
	}
}

func TestProjectViewLargeProjectReadsAreStrictlyBounded(t *testing.T) {
	s := RelationshipState{Projects: make(map[string]ProjectRecord), Agents: make(map[string]AgentRecord)}
	ids := make([]string, 128)
	for i := range ids {
		id := fmt.Sprintf("p-%03d", i)
		ids[i] = id
		s.Projects[id] = ProjectRecord{ID: id, Status: "open"}
		s.Agents["a-"+id] = AgentRecord{ID: "a-" + id, ProjectID: id, Lifecycle: "idle"}
	}
	p := NewProjectViewProjection()
	if err := p.Apply(&dbtx.Tx{}, s); err != nil {
		t.Fatal(err)
	}
	reads, err := p.ReadMany(ids[:16], "", 16)
	if err != nil || len(reads) != 16 {
		t.Fatalf("bounded read = %d results, %v", len(reads), err)
	}
	for _, read := range reads {
		if read.Freshness != ViewFreshnessFresh || read.View == nil || len(read.View.Agents) != 1 {
			t.Fatalf("bad exact-key bounded read: %#v", read)
		}
	}
	if reads, err := p.ReadMany(ids, "", 16); !errors.Is(err, ErrProjectViewReadLimit) || reads != nil {
		t.Fatalf("oversized read = %#v, %v; must reject rather than scan", reads, err)
	}
}
