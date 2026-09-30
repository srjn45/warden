package planexport

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

var updateGoldens = flag.Bool("update", false, "regenerate planexport golden files")

func fixedExportAt() time.Time {
	return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
}

func basePlan(status planstore.PlanStatus) *planstore.Plan {
	p := &planstore.Plan{
		ID:          "plan-deadbeef",
		ProjectID:   "proj-example",
		Name:        "example-plan",
		Goal:        "ship a deterministic replica",
		Constraints: []string{"no secrets in export", "byte-identical re-render"},
		DoneWhen:    []string{"goldens green"},
		Tasks: []planstore.PlanTask{
			{ID: "design", Prompt: "freeze the envelope", After: nil},
			{ID: "render", Prompt: "render YAML", After: []string{"design"}},
			{ID: "verify", Prompt: "check goldens", After: []string{"render"}},
		},
		Status:      status,
		Revision:    3,
		ContentHash: "", // filled below
	}
	planstore.RefreshContentHash(p)
	return p
}

func TestExportPath_descriptiveOnly(t *testing.T) {
	require.Equal(t, "plans/pending/example-plan.yaml", ExportPath(planstore.PlanStatusPending, "example-plan"))
	require.Equal(t, "plans/in_progress/example-plan.yaml", ExportPath(planstore.PlanStatusInProgress, "Example Plan"))
	require.Equal(t, "plans/completed/example-plan.yaml", ExportPath(planstore.PlanStatusCompleted, "example-plan"))
	require.Equal(t, "plans/archived/example-plan.yaml", ExportPath(planstore.PlanStatusArchived, "example-plan"))
}

func TestYAMLRenderer_lifecycleGoldens(t *testing.T) {
	r := YAMLRenderer{}
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
			require.Equal(t, FormatYAML, res.Format)
			require.Equal(t, ExportPath(status, p.Name), res.Path)
			require.Equal(t, status, res.Envelope.Lifecycle)
			require.Nil(t, res.Envelope.ExecutionSummaryRef)
			assertGolden(t, "lifecycle_"+string(status)+".yaml", res.Bytes)
		})
	}
}

func TestYAMLRenderer_taskDAG(t *testing.T) {
	p := basePlan(planstore.PlanStatusPending)
	// Fan-in DAG: two roots, one join.
	p.Tasks = []planstore.PlanTask{
		{ID: "a", Prompt: "first root"},
		{ID: "b", Prompt: "second root"},
		{ID: "join", Prompt: "merge a and b", After: []string{"a", "b"}},
		{ID: "tail", Prompt: "finish", After: []string{"join"}},
	}
	planstore.RefreshContentHash(p)

	res, err := YAMLRenderer{}.Render(p, Options{ExportedAt: fixedExportAt()})
	require.NoError(t, err)
	assertGolden(t, "task_dag.yaml", res.Bytes)
}

func TestYAMLRenderer_executionSummaryRef(t *testing.T) {
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
	// Volatile diagnostics must NOT appear in the replica.
	p.TaskProgress = map[string]string{"design": "done", "render": "done"}
	p.CleanupEvidence = &planstore.CleanupEvidence{
		AttemptedAt: completed,
		Errors:      []string{"/tmp/worktree-xyz failed"},
	}
	p.AutopilotRunID = "run-should-not-export"
	p.Branches = []string{"warden/secret-branch"}

	res, err := YAMLRenderer{}.Render(p, Options{ExportedAt: fixedExportAt()})
	require.NoError(t, err)
	require.NotNil(t, res.Envelope.ExecutionSummaryRef)
	require.Equal(t, "pe-abc12345", res.Envelope.ExecutionSummaryRef.ExecutionID)
	require.NotContains(t, string(res.Bytes), "worktree")
	require.NotContains(t, string(res.Bytes), "run-should-not-export")
	require.NotContains(t, string(res.Bytes), "secret-branch")
	require.NotContains(t, string(res.Bytes), "CleanupEvidence")
	require.NotContains(t, string(res.Bytes), "task_progress")
	assertGolden(t, "execution_summary.yaml", res.Bytes)
}

func TestYAMLRenderer_specialYAMLCharacters(t *testing.T) {
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

	res, err := YAMLRenderer{}.Render(p, Options{ExportedAt: fixedExportAt()})
	require.NoError(t, err)
	assertGolden(t, "special_chars.yaml", res.Bytes)

	// Round-trip: yaml must still decode.
	var doc map[string]any
	require.NoError(t, mustDecodeYAML(res.Bytes, &doc))
	require.Equal(t, p.Goal, doc["goal"])
}

func TestYAMLRenderer_deterministicRerender(t *testing.T) {
	p := basePlan(planstore.PlanStatusInProgress)
	opts := Options{ExportedAt: fixedExportAt()}
	r := YAMLRenderer{}

	a, err := r.Render(p, opts)
	require.NoError(t, err)
	b, err := r.Render(p, opts)
	require.NoError(t, err)
	require.Equal(t, a.Bytes, b.Bytes, "repeat render must be byte-identical")

	// Mutating volatile fields must not change the replica.
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

	assertGolden(t, "deterministic.yaml", a.Bytes)
}

func TestYAMLRenderer_requiresExportedAt(t *testing.T) {
	_, err := YAMLRenderer{}.Render(basePlan(planstore.PlanStatusPending), Options{})
	require.Error(t, err)
}

func TestYAMLRenderer_formatNeutralDefault(t *testing.T) {
	require.Equal(t, FormatYAML, Default().Format())
}

func TestRepoExportMeta(t *testing.T) {
	p := basePlan(planstore.PlanStatusPending)
	meta, err := RepoExportMeta(p, fixedExportAt())
	require.NoError(t, err)
	require.Equal(t, 1, meta.SchemaVersion)
	require.Equal(t, p.Revision, meta.Revision)
	require.Equal(t, p.ContentHash, meta.ContentHash)
	require.Equal(t, "plans/pending/example-plan.yaml", meta.FilePath)
	require.Equal(t, planstore.PlanStatusPending, meta.Lifecycle)
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGoldens {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		t.Logf("updated golden %s", path)
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden %s — run go test -update", path)
	require.Equal(t, string(want), string(got), "golden mismatch for %s", name)
}

func mustDecodeYAML(b []byte, into any) error {
	// strip comment lines then decode
	var cleaned []byte
	for _, line := range splitLines(b) {
		if len(line) > 0 && line[0] == '#' {
			continue
		}
		cleaned = append(cleaned, line...)
		cleaned = append(cleaned, '\n')
	}
	return decodeYAML(cleaned, into)
}

func splitLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			lines = append(lines, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}
