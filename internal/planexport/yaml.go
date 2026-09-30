package planexport

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/srjn45/warden/internal/planstore"
)

// Replica comment prepended to every YAML export so the file is self-describing
// as a non-authoritative replica.
const replicaComment = "# warden-plan-export: replica only — not authoritative\n"

// YAMLRenderer renders the established readable plan YAML plus the frozen
// export envelope. Output is deterministic for identical Plan + Options.
type YAMLRenderer struct{}

// Format implements Renderer.
func (YAMLRenderer) Format() Format { return FormatYAML }

// Render implements Renderer.
func (r YAMLRenderer) Render(plan *planstore.Plan, opts Options) (*Result, error) {
	if plan == nil {
		return nil, fmt.Errorf("planexport: plan is nil")
	}
	env, err := BuildEnvelope(plan, opts.ExportedAt)
	if err != nil {
		return nil, err
	}
	doc := buildYAMLDoc(plan, env)
	body, err := marshalOrdered(doc)
	if err != nil {
		return nil, err
	}
	out := append([]byte(replicaComment), body...)
	return &Result{
		Format:   FormatYAML,
		Bytes:    out,
		Path:     ExportPath(env.Lifecycle, plan.Name),
		Envelope: env,
	}, nil
}

// yamlDoc is the on-disk replica shape: envelope fields first, then the
// established plan body (version/name/goal/constraints/tasks/done_when).
// Field order is the stable key order for yaml.v3 struct marshaling.
type yamlDoc struct {
	SchemaVersion       int           `yaml:"schema_version"`
	PlanID              string        `yaml:"plan_id"`
	Revision            int64         `yaml:"revision"`
	ContentHash         string        `yaml:"content_hash"`
	ExportedAt          string        `yaml:"exported_at"`
	Lifecycle           string        `yaml:"lifecycle"`
	ExecutionSummaryRef any           `yaml:"execution_summary_ref"` // nil → null
	Version             int           `yaml:"version"`
	Name                string        `yaml:"name"`
	Goal                string        `yaml:"goal"`
	Constraints         []string      `yaml:"constraints,omitempty"`
	Tasks               []yamlDocTask `yaml:"tasks"`
	DoneWhen            []string      `yaml:"done_when,omitempty"`
}

type yamlDocTask struct {
	ID     string   `yaml:"id"`
	Prompt string   `yaml:"prompt"`
	After  []string `yaml:"after,omitempty"`
}

type yamlSummaryRef struct {
	PlanID      string `yaml:"plan_id"`
	ExecutionID string `yaml:"execution_id,omitempty"`
	ContentHash string `yaml:"content_hash,omitempty"`
}

func buildYAMLDoc(plan *planstore.Plan, env Envelope) yamlDoc {
	doc := yamlDoc{
		SchemaVersion: env.SchemaVersion,
		PlanID:        env.PlanID,
		Revision:      env.Revision,
		ContentHash:   env.ContentHash,
		ExportedAt:    formatExportedAt(env.ExportedAt),
		Lifecycle:     string(env.Lifecycle),
		Version:       1,
		Name:          plan.Name,
		Goal:          plan.Goal,
		Constraints:   cloneStrings(plan.Constraints),
		Tasks:         make([]yamlDocTask, 0, len(plan.Tasks)),
		DoneWhen:      cloneStrings(plan.DoneWhen),
	}
	if env.ExecutionSummaryRef != nil {
		doc.ExecutionSummaryRef = yamlSummaryRef{
			PlanID:      env.ExecutionSummaryRef.PlanID,
			ExecutionID: env.ExecutionSummaryRef.ExecutionID,
			ContentHash: env.ExecutionSummaryRef.ContentHash,
		}
	}
	// else leave as nil → YAML null
	for _, t := range plan.Tasks {
		doc.Tasks = append(doc.Tasks, yamlDocTask{
			ID:     t.ID,
			Prompt: t.Prompt,
			After:  cloneStrings(t.After),
		})
	}
	return doc
}

func formatExportedAt(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil // omitempty → omit empty slices for established readability
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// marshalOrdered encodes doc with yaml.v3 and normalizes to a single trailing
// newline so repeat renders compare equal as bytes.
func marshalOrdered(doc yamlDoc) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("planexport: encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("planexport: close yaml encoder: %w", err)
	}
	b := buf.Bytes()
	// yaml.v3 Encode already ends with '\n'; strip any extras then ensure one.
	b = bytes.TrimRight(b, "\n")
	b = append(b, '\n')
	// Guard against accidental CRLF from any intermediate tooling.
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n")), nil
}
