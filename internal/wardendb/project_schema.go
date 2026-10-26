package wardendb

// This file is the schema-2 boundary for project-scoped collections.  These
// records deliberately contain only child-to-parent references: inverse child
// lists are a read-model concern and never a persisted authority.

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/srjn45/scriva"
)

const projectSchemaVersion = 3

// ProjectRecord is the canonical project header. Membership is discovered by
// indexed child project_id fields, never by arrays stored here.
type ProjectRecord struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	Status     string `json:"status"`
	GroupID    string `json:"group_id,omitempty"`
	GroupOrder *int   `json:"group_order,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

type AgentRecord struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	PlanID        string `json:"plan_id,omitempty"`
	PipelineID    string `json:"pipeline_id,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	Lifecycle     string `json:"lifecycle"`
	ArchivedAt    string `json:"archived_at,omitempty"`
	ArchivedFrom  string `json:"archived_from_status,omitempty"`
	Scope         string `json:"scope,omitempty"`
	Revision      uint64 `json:"revision"`
}

type TerminalRecord struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id,omitempty"`
	Lifecycle string `json:"lifecycle"`
	Revision  uint64 `json:"revision"`
}
type PlanRecord struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	PipelineID string `json:"pipeline_id,omitempty"`
	Lifecycle  string `json:"lifecycle"`
	Revision   uint64 `json:"revision"`
}
type PipelineRecord struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	PlanID        string `json:"plan_id,omitempty"`
	Lifecycle     string `json:"lifecycle"`
	Revision      uint64 `json:"revision"`
}
type PipelineJobRecord struct {
	ID         string `json:"id"`
	PipelineID string `json:"pipeline_id"`
	AgentID    string `json:"agent_id,omitempty"`
	Lifecycle  string `json:"lifecycle"`
	Revision   uint64 `json:"revision"`
}
type AutopilotRunRecord struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id,omitempty"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	PlanID        string `json:"plan_id,omitempty"`
	Lifecycle     string `json:"lifecycle"`
	Revision      uint64 `json:"revision"`
}

// ProjectCollectionSpecs declares every project-owned collection and its
// unique indexes. Secondary lookup indexes are installed by OpenProjectSchema.
func ProjectCollectionSpecs() []Spec {
	return []Spec{
		{Name: "projects", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id", "path")}},
		{Name: "agents", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
		{Name: "terminals", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
		{Name: "plans", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
		{Name: "pipelines", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
		{Name: "pipeline_jobs", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
		{Name: "autopilot_runs", Version: projectSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
	}
}

// ProjectCollectionIndexes is the complete index contract used by bounded
// project reads. Composite uniqueness and referential checks belong to the
// validator transaction layer, not this schema declaration.
func ProjectCollectionIndexes() map[string][]string {
	return map[string][]string{
		"projects":       {"status", "group_id"},
		"agents":         {"project_id", "parent_agent_id", "plan_id", "pipeline_id", "job_id", "lifecycle"},
		"terminals":      {"project_id", "lifecycle"},
		"plans":          {"project_id", "pipeline_id", "lifecycle"},
		"pipelines":      {"project_id", "parent_agent_id", "plan_id", "lifecycle"},
		"pipeline_jobs":  {"pipeline_id", "agent_id", "lifecycle"},
		"autopilot_runs": {"project_id", "parent_agent_id", "plan_id", "lifecycle"},
	}
}

// RegisterProjectSchema registers the normalized project-scoped collections
// and materializes their lookup indexes. It is intentionally schema-only: it
// does not validate foreign keys or write records. Those operations require a
// cross-collection transaction and are owned by the validator layer.
func RegisterProjectSchema(db *DB) error {
	if err := registerProjectCollection[ProjectRecord](db, "projects"); err != nil {
		return err
	}
	if err := registerProjectCollection[AgentRecord](db, "agents"); err != nil {
		return err
	}
	if err := registerProjectCollection[TerminalRecord](db, "terminals"); err != nil {
		return err
	}
	if err := registerProjectCollection[PlanRecord](db, "plans"); err != nil {
		return err
	}
	if err := registerProjectCollection[PipelineRecord](db, "pipelines"); err != nil {
		return err
	}
	if err := registerProjectCollection[PipelineJobRecord](db, "pipeline_jobs"); err != nil {
		return err
	}
	return registerProjectCollection[AutopilotRunRecord](db, "autopilot_runs")
}

func registerProjectCollection[T any](db *DB, name string) error {
	var spec Spec
	for _, candidate := range ProjectCollectionSpecs() {
		if candidate.Name == name {
			spec = candidate
			break
		}
	}
	if spec.Name == "" {
		return fmt.Errorf("wardendb: missing project schema spec for %q", name)
	}
	col, err := Register[T](db, spec)
	if err != nil {
		return err
	}
	engine, err := col.Engine()
	if err != nil {
		return err
	}
	for _, field := range ProjectCollectionIndexes()[name] {
		if err := engine.EnsureIndex(field); err != nil {
			return fmt.Errorf("wardendb: ensure %s.%s index: %w", name, field, err)
		}
	}
	return nil
}

// CanonicalJSON produces byte-stable JSON for record codecs. json.Marshal
// orders map keys; the compact pass also rejects trailing data on decode.
func CanonicalJSON(v any) ([]byte, error) { return json.Marshal(v) }

// DecodeCanonicalRecord rejects stale inverse-list payloads before decoding a
// schema-2 record. Referential validation is intentionally deferred to Task 2.
func DecodeCanonicalRecord(data []byte, dst any) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, field := range []string{"agents", "pipelines", "terminals", "plans", "autopilots", "project_ids", "child_agents", "child_pipelines", "child_autopilots", "jobs"} {
		if _, found := raw[field]; found {
			return fmt.Errorf("wardendb: forbidden inverse membership field %q", field)
		}
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(canonical, dst)
}

// SortedIndexFields is exposed for deterministic diagnostics and tests.
func SortedIndexFields(collection string) []string {
	fields := append([]string(nil), ProjectCollectionIndexes()[collection]...)
	sort.Strings(fields)
	return fields
}
