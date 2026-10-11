package wardendb

import (
	"strings"
	"testing"
)

func TestProjectSchemaUsesChildForeignKeysAndLookupIndexes(t *testing.T) {
	idx := ProjectCollectionIndexes()
	for collection, field := range map[string]string{"agents": "project_id", "terminals": "project_id", "plans": "project_id", "pipelines": "project_id", "autopilot_runs": "project_id", "pipeline_jobs": "pipeline_id"} {
		found := false
		for _, got := range idx[collection] {
			if got == field {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing %s index: %v", collection, field, idx[collection])
		}
	}
	got := ProjectRecord{ID: "p", Name: "project", Status: "open"}
	if got.ID == "" {
		t.Fatal("invalid fixture")
	}
}

func TestRegisterProjectSchema(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := RegisterProjectSchema(db); err != nil {
		t.Fatal(err)
	}
	registered, err := db.Registered()
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 7 {
		t.Fatalf("registered %d collections, want 7", len(registered))
	}
}

func TestCanonicalCodecIsStableAndRejectsInverseLists(t *testing.T) {
	record := AgentRecord{ID: "agent-1", ProjectID: "project-1", ParentAgentID: "agent-0", Lifecycle: "active", Revision: 7}
	a, err := CanonicalJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CanonicalJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("codec is non-deterministic: %s != %s", a, b)
	}
	var decoded ProjectRecord
	err = DecodeCanonicalRecord([]byte(`{"id":"p","name":"p","status":"open","agents":["a"]}`), &decoded)
	if err == nil || !strings.Contains(err.Error(), "forbidden inverse") {
		t.Fatalf("inverse list accepted: %v", err)
	}
	if err := DecodeCanonicalRecord([]byte(`{"id":"p","name":"p","status":"open"}`), &decoded); err != nil {
		t.Fatal(err)
	}
}
