package autopilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodePlan(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string // substring; "" = expect success
		check   func(t *testing.T, p Plan)
	}{
		{
			name: "minimal valid (goal only)",
			yaml: "version: 1\ngoal: Ship the notifications feature\n",
			check: func(t *testing.T, p Plan) {
				require.Equal(t, 1, p.Version)
				require.Equal(t, "Ship the notifications feature", p.Goal)
			},
		},
		{
			name: "full valid with tasks and edges",
			yaml: "version: 1\ngoal: g\nconstraints: [flagged]\ntasks:\n  - id: api\n    prompt: build api\n  - id: tests\n    prompt: add tests\n    after: [api]\ndone_when:\n  - wd check passes\n",
			check: func(t *testing.T, p Plan) {
				require.Len(t, p.Tasks, 2)
				require.Equal(t, []string{"api"}, p.Tasks[1].After)
				require.Equal(t, []string{"flagged"}, p.Constraints)
				require.Equal(t, []string{"wd check passes"}, p.DoneWhen)
			},
		},
		{
			name:  "version omitted defaults to 1",
			yaml:  "goal: g\n",
			check: func(t *testing.T, p Plan) { require.Equal(t, 1, p.Version) },
		},
		{
			name:    "unknown field rejected (strict decode)",
			yaml:    "version: 1\ngoal: g\ngaol: typo\n",
			wantErr: "gaol",
		},
		{
			name:    "unsupported version",
			yaml:    "version: 2\ngoal: g\n",
			wantErr: "unsupported version",
		},
		{
			name:    "missing goal",
			yaml:    "version: 1\nconstraints: [x]\n",
			wantErr: "goal is required",
		},
		{
			name:    "empty goal",
			yaml:    "version: 1\ngoal: '   '\n",
			wantErr: "goal is required",
		},
		{
			name:    "duplicate task id",
			yaml:    "goal: g\ntasks:\n  - id: a\n    prompt: x\n  - id: a\n    prompt: y\n",
			wantErr: "duplicate task id",
		},
		{
			name:    "empty task id",
			yaml:    "goal: g\ntasks:\n  - id: ''\n    prompt: x\n",
			wantErr: "empty id",
		},
		{
			name:    "after references unknown id",
			yaml:    "goal: g\ntasks:\n  - id: a\n    prompt: x\n    after: [ghost]\n",
			wantErr: "unknown id",
		},
		{
			name:  "forward reference is legal",
			yaml:  "goal: g\ntasks:\n  - id: a\n    prompt: x\n    after: [b]\n  - id: b\n    prompt: y\n",
			check: func(t *testing.T, p Plan) { require.Len(t, p.Tasks, 2) },
		},
		{
			// The completion marker must strict-decode (the KnownFields decoder would
			// otherwise reject `status`/`completed_at`), and mark the plan complete.
			name: "completion marker decodes and is complete",
			yaml: "version: 1\ngoal: g\nstatus: complete\ncompleted_at: 2026-07-21T10:00:00Z\n",
			check: func(t *testing.T, p Plan) {
				require.Equal(t, "complete", p.Status)
				require.Equal(t, "2026-07-21T10:00:00Z", p.CompletedAt)
				require.True(t, p.IsComplete())
			},
		},
		{
			name: "absent status is active (not complete)",
			yaml: "version: 1\ngoal: g\n",
			check: func(t *testing.T, p Plan) {
				require.Empty(t, p.Status)
				require.False(t, p.IsComplete())
			},
		},
		{
			// A hand-typed non-"complete" status must never block loading nor read as
			// complete — Status is lenient by design.
			name: "unknown status value still loads and is not complete",
			yaml: "version: 1\ngoal: g\nstatus: paused\n",
			check: func(t *testing.T, p Plan) {
				require.Equal(t, "paused", p.Status)
				require.False(t, p.IsComplete())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := DecodePlan([]byte(tt.yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			if tt.check != nil {
				tt.check(t, p)
			}
		})
	}
}

func TestPlanTaskStatusValidationAndDefaults(t *testing.T) {
	p, err := DecodePlan([]byte("goal: g\ntasks:\n  - id: a\n    prompt: x\n"))
	require.NoError(t, err)
	require.Equal(t, TaskStatusPending, p.Tasks[0].Status)
	_, err = DecodePlan([]byte("goal: g\ntasks:\n  - id: a\n    prompt: x\n    status: done\n"))
	require.ErrorContains(t, err, "requires landed_pr")
	_, err = DecodePlan([]byte("goal: g\ntasks:\n  - id: a\n    prompt: x\n    status: active\n    landed_pr: 4\n"))
	require.ErrorContains(t, err, "only valid")
}

func TestWriteTaskStatusAtomicPreservesPlanAndReloadsLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.yaml")
	require.NoError(t, os.WriteFile(path, []byte("# owner\ngoal: g\ntasks:\n  - id: a # keep\n    prompt: x\n"), 0o640))
	require.NoError(t, writeTaskStatusAtomic(path, "a", TaskStatusDone, 42))
	p, err := LoadPlan(path)
	require.NoError(t, err)
	require.Equal(t, TaskStatusDone, p.Tasks[0].Status)
	require.Equal(t, 42, p.Tasks[0].LandedPR)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(b), "# owner")
	require.Contains(t, string(b), "# keep")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestLoadPlan(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(good, []byte("version: 1\ngoal: g\n"), 0o644))

	p, err := LoadPlan(good)
	require.NoError(t, err)
	require.Equal(t, "g", p.Goal)

	_, err = LoadPlan(filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "plan file not found")
}

func TestValidateStructuralVsContent(t *testing.T) {
	t.Run("structural: empty goal", func(t *testing.T) {
		p := Plan{Version: 1}
		require.Error(t, p.validateStructural())
		require.Contains(t, p.validateStructural().Error(), "goal is required")
	})
	t.Run("structural: bad version", func(t *testing.T) {
		p := Plan{Version: 99, Goal: "g"}
		require.Error(t, p.validateStructural())
		require.Contains(t, p.validateStructural().Error(), "unsupported version")
	})
	t.Run("structural: duplicate task ids", func(t *testing.T) {
		p := Plan{Version: 1, Goal: "g", Tasks: []PlanTask{{ID: "a"}, {ID: "a"}}}
		require.Error(t, p.validateStructural())
		require.Contains(t, p.validateStructural().Error(), "duplicate task id")
	})
	t.Run("structural: bad dep edge", func(t *testing.T) {
		p := Plan{Version: 1, Goal: "g", Tasks: []PlanTask{{ID: "a", After: []string{"ghost"}}}}
		require.Error(t, p.validateStructural())
		require.Contains(t, p.validateStructural().Error(), "unknown id")
	})
	t.Run("structural: valid plan passes", func(t *testing.T) {
		p := Plan{Version: 1, Goal: "g", Tasks: []PlanTask{{ID: "a"}, {ID: "b", After: []string{"a"}}}}
		require.NoError(t, p.validateStructural())
	})

	t.Run("content: invalid status string", func(t *testing.T) {
		p := Plan{Version: 1, Goal: "g", Tasks: []PlanTask{{ID: "a", Status: "completed"}}}
		require.NoError(t, p.validateStructural()) // structure is fine
		fails := p.validateContent()
		require.NotEmpty(t, fails)
		require.Contains(t, fails[0], "invalid status")
	})
	t.Run("content: done without landed_pr", func(t *testing.T) {
		p := Plan{Version: 1, Goal: "g", Tasks: []PlanTask{{ID: "a", Status: "done", LandedPR: 0}}}
		require.NoError(t, p.validateStructural())
		fails := p.validateContent()
		require.NotEmpty(t, fails)
		require.Contains(t, fails[0], "requires landed_pr")
	})
	t.Run("content: valid plan has no content failures", func(t *testing.T) {
		p := Plan{Version: 1, Goal: "g", Tasks: []PlanTask{{ID: "a", Status: "pending"}}}
		require.NoError(t, p.validateStructural())
		require.Empty(t, p.validateContent())
	})
}

func TestLoadPlanLenient(t *testing.T) {
	t.Run("normalizes invalid status to pending", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plan.yaml")
		require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: g\ntasks:\n  - id: a\n    prompt: x\n    status: completed\n"), 0o644))
		plan, warnings, err := loadPlanLenient(path)
		require.NoError(t, err)
		require.Equal(t, TaskStatusPending, plan.Tasks[0].Status)
		require.NotEmpty(t, warnings)
		require.Contains(t, warnings[0], "task a")
		require.Contains(t, warnings[0], "normalized to pending")
	})
	t.Run("normalizes done without landed_pr to pending", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plan.yaml")
		require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: g\ntasks:\n  - id: a\n    prompt: x\n    status: done\n"), 0o644))
		plan, warnings, err := loadPlanLenient(path)
		require.NoError(t, err)
		require.Equal(t, TaskStatusPending, plan.Tasks[0].Status)
		require.Equal(t, 0, plan.Tasks[0].LandedPR)
		require.NotEmpty(t, warnings)
	})
	t.Run("structural error: missing file", func(t *testing.T) {
		_, _, err := loadPlanLenient("/nonexistent/path/plan.yaml")
		require.Error(t, err)
		require.Contains(t, err.Error(), "plan file not found")
	})
	t.Run("structural error: bad YAML", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plan.yaml")
		require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: g\nbogus_field: 1\n"), 0o644))
		_, _, err := loadPlanLenient(path)
		require.Error(t, err)
	})
	t.Run("valid plan returns no warnings", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "plan.yaml")
		require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: g\n"), 0o644))
		plan, warnings, err := loadPlanLenient(path)
		require.NoError(t, err)
		require.Empty(t, warnings)
		require.Equal(t, "g", plan.Goal)
	})
}

// TestMarkPlanCompleteInPlace verifies the in-place completion marker: the
// rewritten file still strict-decodes, carries the marker, and — critically —
// preserves an owner's pre-existing comment and the other keys.
func TestMarkPlanCompleteInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.yaml")
	original := "# owner's steering notes — keep me!\n" +
		"version: 1\n" +
		"goal: Ship the notifications feature # inline comment\n" +
		"constraints:\n" +
		"  - all changes behind a feature flag\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	require.NoError(t, markPlanCompleteInPlace(path, "2026-07-21T10:00:00Z"))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)

	// The comment (both head and inline) must survive the round-trip.
	require.Contains(t, text, "# owner's steering notes — keep me!")
	require.Contains(t, text, "# inline comment")

	// The rewritten file must still strict-decode and now be complete.
	p, err := LoadPlan(path)
	require.NoError(t, err)
	require.True(t, p.IsComplete())
	require.Equal(t, "complete", p.Status)
	require.Equal(t, "2026-07-21T10:00:00Z", p.CompletedAt)
	// Existing keys untouched.
	require.Equal(t, "Ship the notifications feature", p.Goal)
	require.Equal(t, []string{"all changes behind a feature flag"}, p.Constraints)

	// Idempotent-ish: re-marking updates in place (no duplicate keys) and still
	// decodes cleanly with the new timestamp.
	require.NoError(t, markPlanCompleteInPlace(path, "2026-07-21T11:30:00Z"))
	raw2, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(raw2), "status:"))
	require.Equal(t, 1, strings.Count(string(raw2), "completed_at:"))
	p2, err := LoadPlan(path)
	require.NoError(t, err)
	require.Equal(t, "2026-07-21T11:30:00Z", p2.CompletedAt)
}
