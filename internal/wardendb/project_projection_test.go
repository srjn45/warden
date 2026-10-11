package wardendb

import (
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/dbtx"
)

func projectionState() RelationshipState {
	return RelationshipState{
		Projects: map[string]ProjectRecord{
			"p1": {ID: "p1", Name: "one", Status: "open"},
			"p2": {ID: "p2", Name: "two", Status: "open"},
		},
		Agents: map[string]AgentRecord{
			"a2":      {ID: "a2", ProjectID: "p1", Lifecycle: "busy"},
			"a1":      {ID: "a1", ProjectID: "p1", Lifecycle: "idle"},
			"outside": {ID: "outside", ProjectID: "missing", Lifecycle: "busy"},
		},
		Terminals: map[string]TerminalRecord{"t1": {ID: "t1", ProjectID: "p1", Lifecycle: "busy"}},
		Plans:     map[string]PlanRecord{"plan": {ID: "plan", ProjectID: "p1", Lifecycle: "active"}},
		Pipelines: map[string]PipelineRecord{"pipe": {ID: "pipe", ProjectID: "p1", Lifecycle: "running"}},
		Jobs: map[string]PipelineJobRecord{
			"j2":     {ID: "j2", PipelineID: "pipe", Lifecycle: "pending"},
			"j1":     {ID: "j1", PipelineID: "pipe", Lifecycle: "busy"},
			"orphan": {ID: "orphan", PipelineID: "missing", Lifecycle: "busy"},
		},
		Runs: map[string]AutopilotRunRecord{"run": {ID: "run", ProjectID: "p1", Lifecycle: "running"}},
	}
}

func TestProjectViewProjectionUpdatesAtomicallyFromCanonicalState(t *testing.T) {
	p := NewProjectViewProjection()
	if err := p.Apply(nil, projectionState()); !errors.Is(err, ErrProjectionTransaction) {
		t.Fatalf("Apply without transaction = %v, want transaction error", err)
	}
	if err := p.Apply(&dbtx.Tx{}, projectionState()); err != nil {
		t.Fatal(err)
	}
	v, ok := p.View("p1")
	if !ok {
		t.Fatal("missing p1 view")
	}
	if got := []string{v.Agents[0].ID, v.Agents[1].ID}; got[0] != "a1" || got[1] != "a2" {
		t.Fatalf("agents not deterministically ordered: %v", got)
	}
	if len(v.Jobs) != 2 || v.Jobs[0].ID != "j1" || v.Counters.Agents["busy"] != 1 || v.Counters.Jobs["pending"] != 1 {
		t.Fatalf("bad derived view: %#v", v)
	}
	if _, ok := p.View("missing"); ok {
		t.Fatal("view was constructed from dangling child project id")
	}
	// Returned views are copies: callers cannot mutate the materialized cache.
	v.Agents[0].ID = "mutated"
	again, _ := p.View("p1")
	if again.Agents[0].ID != "a1" {
		t.Fatal("caller mutated projection cache")
	}
}

func TestProjectViewRebuildIsDeterministicAndComparable(t *testing.T) {
	s := projectionState()
	a, err := RebuildProjectViews(s)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RebuildProjectViews(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareProjectViews(a, s); err != nil {
		t.Fatalf("equivalent rebuild: %v", err)
	}
	if a["p1"].Revision != b["p1"].Revision {
		t.Fatalf("non-deterministic revision: %q != %q", a["p1"].Revision, b["p1"].Revision)
	}
	a["p1"] = ProjectView{}
	if err := CompareProjectViews(a, s); err == nil {
		t.Fatal("stale view compared equal")
	}
}

func TestProjectViewComparisonRejectsUnexpectedCachedProject(t *testing.T) {
	views, err := RebuildProjectViews(projectionState())
	if err != nil {
		t.Fatal(err)
	}
	views["ghost"] = ProjectView{}
	if err := CompareProjectViews(views, projectionState()); err == nil {
		t.Fatal("unexpected view compared equal")
	}
}

func TestProjectViewProjectionReadReportsFreshnessWithoutFallback(t *testing.T) {
	p := NewProjectViewProjection()
	if err := p.Apply(&dbtx.Tx{}, projectionState()); err != nil {
		t.Fatal(err)
	}
	v, ok := p.View("p1")
	if !ok {
		t.Fatal("missing p1 view")
	}

	fresh := p.Read("p1", v.Revision)
	if fresh.Freshness != ViewFreshnessFresh || fresh.View == nil || fresh.Revision != v.Revision {
		t.Fatalf("fresh read = %#v", fresh)
	}
	stale := p.Read("p1", "pv:old")
	if stale.Freshness != ViewFreshnessStale || stale.View == nil {
		t.Fatalf("stale read = %#v; stale is metadata, not a reason to invent a fallback aggregate", stale)
	}
	missing := p.Read("absent", "")
	if missing.Freshness != ViewFreshnessMissing || missing.View != nil {
		t.Fatalf("missing read = %#v; missing views must not expose an authoritative-looking empty child list", missing)
	}
}

func TestProjectViewProjectionReadManyIsBounded(t *testing.T) {
	p := NewProjectViewProjection()
	if reads, err := p.ReadMany([]string{"p1", "p2"}, "", 1); !errors.Is(err, ErrProjectViewReadLimit) || reads != nil {
		t.Fatalf("over-limit ReadMany = %#v, %v", reads, err)
	}
	reads, err := p.ReadMany([]string{"p1", "p1"}, "", 2)
	if err != nil || len(reads) != 2 || reads[0].ProjectID != "p1" || reads[1].ProjectID != "p1" {
		t.Fatalf("bounded positional ReadMany = %#v, %v", reads, err)
	}
	if _, err := p.ReadMany(nil, "", -1); !errors.Is(err, ErrProjectViewReadLimit) {
		t.Fatalf("negative bound error = %v", err)
	}
}
