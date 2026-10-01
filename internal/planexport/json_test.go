package planexport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

// envelopeParityKeys are the frozen export envelope fields that YAML and JSON
// replicas must carry with identical values (spec §8.1 / §10).
var envelopeParityKeys = []string{
	"schema_version",
	"plan_id",
	"revision",
	"content_hash",
	"exported_at",
	"lifecycle",
	"execution_summary_ref",
}

func TestExportPathFormat_json(t *testing.T) {
	require.Equal(t, "plans/pending/example-plan.json",
		ExportPathFormat(planstore.PlanStatusPending, "example-plan", FormatJSON))
	require.Equal(t, "plans/in_progress/example-plan.json",
		ExportPathFormat(planstore.PlanStatusInProgress, "Example Plan", FormatJSON))
	require.Equal(t, "plans/pending/example-plan.yaml",
		ExportPathFormat(planstore.PlanStatusPending, "example-plan", FormatYAML))
	// ExportPath remains the YAML conventional path.
	require.Equal(t, ExportPath(planstore.PlanStatusPending, "example-plan"),
		ExportPathFormat(planstore.PlanStatusPending, "example-plan", FormatYAML))
}

func TestJSONRenderer_lifecycleGoldens(t *testing.T) {
	r := JSONRenderer{}
	opts := Options{ExportedAt: fixedExportAt()}
	for _, status := range []planstore.PlanStatus{
		planstore.PlanStatusPending,
		planstore.PlanStatusInProgress,
		planstore.PlanStatusCompleted,
		planstore.PlanStatusArchived,
	} {
		t.Run(string(status), func(t *testing.T) {
			p := basePlan(status)
			res, err := r.Render(p, opts)
			require.NoError(t, err)
			require.Equal(t, FormatJSON, res.Format)
			require.Equal(t, ExportPathFormat(status, p.Name, FormatJSON), res.Path)
			require.Equal(t, status, res.Envelope.Lifecycle)
			require.Nil(t, res.Envelope.ExecutionSummaryRef)
			require.Contains(t, string(res.Bytes), `"warden_plan_export": "replica only — not authoritative"`)
			assertGolden(t, "lifecycle_"+string(status)+".json", res.Bytes)
		})
	}
}

func TestJSONRenderer_taskDAG(t *testing.T) {
	p := basePlan(planstore.PlanStatusPending)
	p.Tasks = []planstore.PlanTask{
		{ID: "a", Prompt: "first root"},
		{ID: "b", Prompt: "second root"},
		{ID: "join", Prompt: "merge a and b", After: []string{"a", "b"}},
		{ID: "tail", Prompt: "finish", After: []string{"join"}},
	}
	planstore.RefreshContentHash(p)

	res, err := JSONRenderer{}.Render(p, Options{ExportedAt: fixedExportAt()})
	require.NoError(t, err)
	assertGolden(t, "task_dag.json", res.Bytes)
}

func TestJSONRenderer_executionSummaryRef(t *testing.T) {
	p := basePlan(planstore.PlanStatusCompleted)
	completed := fixedExportAt().Add(-time.Hour)
	p.ExecutionSummary = &planstore.ExecutionSummary{
		PlanID:        p.ID,
		PlanName:      p.Name,
		Goal:          p.Goal,
		ExecutionMode: planstore.PlanModePipeline,
		ExecutorID:    "pipe-1",
		StartedAt:     completed.Add(-2 * time.Hour),
		CompletedAt:   &completed,
		TasksTotal:    3,
		TasksDone:     3,
		OutcomeNote:   "all tasks done",
	}
	p.ExecutionHistory = []planstore.PlanExecution{{
		ID:            "pe-abc12345",
		PlanID:        p.ID,
		ExecutionMode: planstore.PlanModePipeline,
		ExecutorID:    "pipe-1",
		StartedAt:     completed.Add(-2 * time.Hour),
		CompletedAt:   &completed,
	}}
	p.TaskProgress = map[string]string{"design": "done", "render": "done"}
	p.CleanupEvidence = &planstore.CleanupEvidence{
		AttemptedAt: completed,
		Errors:      []string{"/tmp/worktree-xyz failed"},
	}
	p.AutopilotRunID = "run-should-not-export"
	p.Branches = []string{"warden/secret-branch"}

	res, err := JSONRenderer{}.Render(p, Options{ExportedAt: fixedExportAt()})
	require.NoError(t, err)
	require.NotNil(t, res.Envelope.ExecutionSummaryRef)
	require.Equal(t, "pe-abc12345", res.Envelope.ExecutionSummaryRef.ExecutionID)
	require.NotContains(t, string(res.Bytes), "worktree")
	require.NotContains(t, string(res.Bytes), "run-should-not-export")
	require.NotContains(t, string(res.Bytes), "secret-branch")
	require.NotContains(t, string(res.Bytes), "CleanupEvidence")
	require.NotContains(t, string(res.Bytes), "task_progress")
	assertGolden(t, "execution_summary.json", res.Bytes)
}

func TestJSONRenderer_specialCharacters(t *testing.T) {
	p := basePlan(planstore.PlanStatusPending)
	p.Name = "special-chars"
	p.Goal = "Handle: colons, \"quotes\", and\nnewlines — plus unicode café ☕"
	p.Constraints = []string{
		`raw: "quoted" & ampersand`,
		"line1\nline2\ttab",
		"* leading asterisk",
	}
	p.DoneWhen = []string{`done when: it's "finished"`}
	p.Tasks = []planstore.PlanTask{
		{
			ID:     "quote-task",
			Prompt: "Use `code`, $HOME, and a: mapping lookalike",
			After:  nil,
		},
		{
			ID:     "block-task",
			Prompt: "Multi-line prompt:\n- item one\n- item two: value\n",
			After:  []string{"quote-task"},
		},
	}
	planstore.RefreshContentHash(p)

	res, err := JSONRenderer{}.Render(p, Options{ExportedAt: fixedExportAt()})
	require.NoError(t, err)
	assertGolden(t, "special_chars.json", res.Bytes)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(res.Bytes, &doc))
	require.Equal(t, p.Goal, doc["goal"])
	require.Equal(t, jsonReplicaMarker, doc[jsonReplicaMarkerKey])
}

func TestJSONRenderer_deterministicRerender(t *testing.T) {
	p := basePlan(planstore.PlanStatusInProgress)
	opts := Options{ExportedAt: fixedExportAt()}
	r := JSONRenderer{}

	a, err := r.Render(p, opts)
	require.NoError(t, err)
	b, err := r.Render(p, opts)
	require.NoError(t, err)
	require.Equal(t, a.Bytes, b.Bytes, "repeat render must be byte-identical")

	p.TaskProgress = map[string]string{"design": "in_progress"}
	p.UpdatedAt = time.Now().UTC()
	p.ActiveExecution = &planstore.PlanExecution{
		ID:        "pe-volatile",
		PlanID:    p.ID,
		StartedAt: time.Now().UTC(),
	}
	c, err := r.Render(p, opts)
	require.NoError(t, err)
	require.Equal(t, a.Bytes, c.Bytes)

	assertGolden(t, "deterministic.json", a.Bytes)
}

func TestJSONRenderer_requiresExportedAt(t *testing.T) {
	_, err := JSONRenderer{}.Render(basePlan(planstore.PlanStatusPending), Options{})
	require.Error(t, err)
}

func TestJSONRenderer_defaultRemainsYAML(t *testing.T) {
	require.Equal(t, FormatYAML, Default().Format())
	_, isYAML := Default().(YAMLRenderer)
	require.True(t, isYAML)
}

// TestJSONRenderer_envelopeFieldParityWithYAML proves JSON and YAML replicas
// carry the same §8.1 envelope field values (plus matching definition body
// fields). The JSON marker is the only format-specific top-level key.
func TestJSONRenderer_envelopeFieldParityWithYAML(t *testing.T) {
	cases := []struct {
		name string
		plan *planstore.Plan
	}{
		{"pending", basePlan(planstore.PlanStatusPending)},
		{"in_progress", basePlan(planstore.PlanStatusInProgress)},
		{"with_summary", func() *planstore.Plan {
			p := basePlan(planstore.PlanStatusCompleted)
			completed := fixedExportAt().Add(-time.Hour)
			p.ExecutionSummary = &planstore.ExecutionSummary{
				PlanID: p.ID, PlanName: p.Name, Goal: p.Goal,
				ExecutionMode: planstore.PlanModePipeline, ExecutorID: "pipe-1",
				StartedAt: completed.Add(-2 * time.Hour), CompletedAt: &completed,
				TasksTotal: 3, TasksDone: 3,
			}
			p.ExecutionHistory = []planstore.PlanExecution{{
				ID: "pe-abc12345", PlanID: p.ID,
				ExecutionMode: planstore.PlanModePipeline, ExecutorID: "pipe-1",
				StartedAt: completed.Add(-2 * time.Hour), CompletedAt: &completed,
			}}
			return p
		}()},
		{"task_dag", func() *planstore.Plan {
			p := basePlan(planstore.PlanStatusPending)
			p.Tasks = []planstore.PlanTask{
				{ID: "a", Prompt: "first root"},
				{ID: "b", Prompt: "second root"},
				{ID: "join", Prompt: "merge a and b", After: []string{"a", "b"}},
			}
			planstore.RefreshContentHash(p)
			return p
		}()},
	}

	opts := Options{ExportedAt: fixedExportAt()}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yRes, err := YAMLRenderer{}.Render(tc.plan, opts)
			require.NoError(t, err)
			jRes, err := JSONRenderer{}.Render(tc.plan, opts)
			require.NoError(t, err)

			require.Equal(t, yRes.Envelope, jRes.Envelope, "typed Envelope must match")

			yDoc := map[string]any{}
			require.NoError(t, mustDecodeYAML(yRes.Bytes, &yDoc))
			jDoc := map[string]any{}
			require.NoError(t, json.Unmarshal(jRes.Bytes, &jDoc))

			require.Equal(t, jsonReplicaMarker, jDoc[jsonReplicaMarkerKey])
			_, yamlHasMarker := yDoc[jsonReplicaMarkerKey]
			require.False(t, yamlHasMarker, "YAML uses a comment header, not warden_plan_export")

			for _, key := range envelopeParityKeys {
				require.Contains(t, yDoc, key, "YAML missing %s", key)
				require.Contains(t, jDoc, key, "JSON missing %s", key)
				require.Equal(t, normalizeJSONNumber(yDoc[key]), normalizeJSONNumber(jDoc[key]),
					"parity mismatch for envelope field %s", key)
			}

			// Definition body parity (same keys/values as YAML body).
			for _, key := range []string{"version", "name", "goal", "tasks"} {
				require.Equal(t, normalizeJSONNumber(yDoc[key]), normalizeJSONNumber(jDoc[key]),
					"parity mismatch for body field %s", key)
			}
			for _, key := range []string{"constraints", "done_when"} {
				require.Equal(t, normalizeJSONNumber(yDoc[key]), normalizeJSONNumber(jDoc[key]),
					"parity mismatch for optional body field %s", key)
			}
		})
	}
}

// normalizeJSONNumber converts yaml.v3 ints and json.Number/float64 to a
// comparable form so envelope parity asserts don't fail on numeric type drift.
func normalizeJSONNumber(v any) any {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[k] = normalizeJSONNumber(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = normalizeJSONNumber(vv)
		}
		return out
	default:
		return v
	}
}
