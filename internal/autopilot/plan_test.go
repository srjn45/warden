package autopilot

import (
	"os"
	"path/filepath"
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
