package planexport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/planstore"
)

// JSON replica marker: a top-level field (JSON has no comment syntax) that
// mirrors the YAML "# warden-plan-export: …" header so collision detection and
// humans can recognize an inert warden export.
const (
	jsonReplicaMarkerKey = "warden_plan_export"
	jsonReplicaMarker    = "replica only — not authoritative"
)

// JSONRenderer renders the same export envelope + definition body as
// YAMLRenderer, encoded as deterministic JSON. Path uses the .json extension.
type JSONRenderer struct{}

// Format implements Renderer.
func (JSONRenderer) Format() Format { return FormatJSON }

// Render implements Renderer.
func (r JSONRenderer) Render(plan *planstore.Plan, opts Options) (*Result, error) {
	if plan == nil {
		return nil, fmt.Errorf("planexport: plan is nil")
	}
	env, err := BuildEnvelope(plan, opts.ExportedAt)
	if err != nil {
		return nil, err
	}
	doc := buildJSONDoc(plan, env)
	body, err := marshalJSONOrdered(doc)
	if err != nil {
		return nil, err
	}
	return &Result{
		Format:   FormatJSON,
		Bytes:    body,
		Path:     ExportPathFormat(env.Lifecycle, plan.Name, FormatJSON),
		Envelope: env,
	}, nil
}

// jsonDoc is the on-disk JSON replica shape: replica marker, envelope fields,
// then the established plan body. Field order is the stable key order for
// encoding/json struct marshaling.
type jsonDoc struct {
	WardenPlanExport    string        `json:"warden_plan_export"`
	SchemaVersion       int           `json:"schema_version"`
	PlanID              string        `json:"plan_id"`
	Revision            int64         `json:"revision"`
	ContentHash         string        `json:"content_hash"`
	ExportedAt          string        `json:"exported_at"`
	Lifecycle           string        `json:"lifecycle"`
	ExecutionSummaryRef any           `json:"execution_summary_ref"` // nil → null
	Version             int           `json:"version"`
	Name                string        `json:"name"`
	Goal                string        `json:"goal"`
	Constraints         []string      `json:"constraints,omitempty"`
	Tasks               []jsonDocTask `json:"tasks"`
	DoneWhen            []string      `json:"done_when,omitempty"`
}

type jsonDocTask struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	After  []string `json:"after,omitempty"`
}

type jsonSummaryRef struct {
	PlanID      string `json:"plan_id"`
	ExecutionID string `json:"execution_id,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

func buildJSONDoc(plan *planstore.Plan, env Envelope) jsonDoc {
	doc := jsonDoc{
		WardenPlanExport: jsonReplicaMarker,
		SchemaVersion:    env.SchemaVersion,
		PlanID:           env.PlanID,
		Revision:         env.Revision,
		ContentHash:      env.ContentHash,
		ExportedAt:       formatExportedAt(env.ExportedAt),
		Lifecycle:        string(env.Lifecycle),
		Version:          1,
		Name:             plan.Name,
		Goal:             plan.Goal,
		Constraints:      cloneStrings(plan.Constraints),
		Tasks:            make([]jsonDocTask, 0, len(plan.Tasks)),
		DoneWhen:         cloneStrings(plan.DoneWhen),
	}
	if env.ExecutionSummaryRef != nil {
		doc.ExecutionSummaryRef = jsonSummaryRef{
			PlanID:      env.ExecutionSummaryRef.PlanID,
			ExecutionID: env.ExecutionSummaryRef.ExecutionID,
			ContentHash: env.ExecutionSummaryRef.ContentHash,
		}
	}
	for _, t := range plan.Tasks {
		doc.Tasks = append(doc.Tasks, jsonDocTask{
			ID:     t.ID,
			Prompt: t.Prompt,
			After:  cloneStrings(t.After),
		})
	}
	return doc
}

// marshalJSONOrdered encodes doc with stable indent and a single trailing
// newline so repeat renders compare equal as bytes.
func marshalJSONOrdered(doc jsonDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("planexport: encode json: %w", err)
	}
	// Encoder.Encode already appends '\n'; normalize anyway.
	b := bytes.TrimRight(buf.Bytes(), "\n")
	b = append(b, '\n')
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n")), nil
}
