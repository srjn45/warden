package tree

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// RFC §18 canonical populated tree golden test
func TestGolden_RFC18PopulatedTree(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	in := Inputs{
		Projects: []projectstore.Project{
			{
				ID:     "/home/u/dev/warden",
				Name:   "warden",
				Path:   "/home/u/dev/warden",
				Status: projectstore.StatusOpen,
			},
		},
		Autopilot: autopilot.Status{
			Enabled: true,
			Runs: []autopilot.RunStatus{
				{
					RunID:      "ap-42",
					Name:       "recovery-finish",
					Repo:       "/home/u/dev/warden",
					State:      autopilot.StateActive,
					Gate:       "auto",
					Brain:      &autopilot.BrainStatus{AgentID: "ap-42-brain"},
					GuardianID: "ap-42-guard",
					PlanTasks: []autopilot.PlanTask{
						{
							ID:     "t1",
							Prompt: "wire resolver into first-spawn",
						},
					},
					LedgerTasks: []autopilot.LedgerTask{
						{
							ID: "t1",
						},
					},
				},
			},
		},
		Pipelines: []*pipeline.Pipeline{
			{
				ID:     "cred-inject",
				Name:   "cred-inject",
				Repo:   "/home/u/dev/warden",
				Status: pipeline.StatusRunning,
				Jobs: []pipeline.Job{
					{
						ID:        "implement",
						Status:    pipeline.JobRunning,
						SessionID: "impl-1",
						DependsOn: []string{},
					},
					{
						ID:        "review",
						Status:    pipeline.JobPending,
						DependsOn: []string{"implement"},
					},
				},
			},
		},
		Sessions: []*store.Session{
			// Autopilot manager
			{
				ID:             "ap-42-brain",
				AutopilotRunID: "ap-42",
				AutopilotSlot:  store.AutopilotSlotManager,
				Status:         store.StatusWorking,
				Kind:           store.KindAgent,
				CreatedAt:      now.Add(1 * time.Minute),
			},
			// Autopilot guardian
			{
				ID:             "ap-42-guard",
				AutopilotRunID: "ap-42",
				AutopilotSlot:  store.AutopilotSlotGuardian,
				Status:         store.StatusIdle,
				Kind:           store.KindAgent,
				CreatedAt:      now.Add(2 * time.Minute),
			},
			// Autopilot worker
			{
				ID:              "w-9",
				Name:            "worker-9",
				AutopilotRunID:  "ap-42",
				AutopilotSlot:   store.AutopilotSlotWorker,
				AutopilotTaskID: "t1",
				Status:          store.StatusWorking,
				Kind:            store.KindAgent,
				CreatedAt:       now.Add(3 * time.Minute),
			},
			// Pipeline job session
			{
				ID:         "impl-1",
				PipelineID: "cred-inject",
				JobID:      "implement",
				Status:     store.StatusWorking,
				Kind:       store.KindAgent,
				CreatedAt:  now.Add(4 * time.Minute),
			},
			// Root agent
			{
				ID:        "agent-7",
				Name:      "orch-warden",
				ProjectID: "/home/u/dev/warden",
				Repo:      "/home/u/dev/warden",
				Backend:   "claude",
				Status:    store.StatusWaitingForInput,
				Kind:      store.KindAgent,
				CreatedAt: now.Add(5 * time.Minute),
			},
			// Child agent
			{
				ID:        "agent-8",
				Name:      "sub-explorer",
				ParentID:  "agent-7",
				ProjectID: "/home/u/dev/warden",
				Repo:      "/home/u/dev/warden",
				Backend:   "claude",
				Status:    store.StatusWorking,
				Kind:      store.KindAgent,
				CreatedAt: now.Add(6 * time.Minute),
			},
			// Terminal under project
			{
				ID:        "term-3",
				Name:      "warden ~ main",
				ProjectID: "/home/u/dev/warden",
				Repo:      "/home/u/dev/warden",
				Status:    store.StatusIdle,
				Kind:      store.KindTerminal,
				CreatedAt: now.Add(7 * time.Minute),
			},
			// Bare terminal in No project bucket
			{
				ID:        "term-9",
				Name:      "shell",
				Status:    store.StatusIdle,
				Kind:      store.KindTerminal,
				CreatedAt: now.Add(8 * time.Minute),
			},
		},
	}

	svc := NewService()
	tree := svc.Build(in, "")

	gotJSON, err := json.MarshalIndent(tree, "", "  ")
	require.NoError(t, err)

	expectedJSON := `{
  "roots": [
    {
      "type": "project",
      "id": "project:/home/u/dev/warden",
      "label": "warden",
      "status": "active",
      "detail": {
        "repo": "/home/u/dev/warden",
        "path": "/home/u/dev/warden"
      },
      "children": [
        {
          "type": "autopilot_run",
          "id": "run:ap-42",
          "label": "recovery-finish",
          "status": "active",
          "detail": {
            "repo": "/home/u/dev/warden",
            "gate": "auto"
          },
          "children": [
            {
              "type": "manager",
              "id": "session:ap-42-brain",
              "label": "manager",
              "status": "active",
              "session_id": "ap-42-brain",
              "detail": {
                "kind": "agent",
                "slot": "autopilot"
              }
            },
            {
              "type": "guardian",
              "id": "session:ap-42-guard",
              "label": "guardian",
              "status": "idle",
              "session_id": "ap-42-guard",
              "detail": {
                "kind": "agent",
                "slot": "guardian"
              }
            },
            {
              "type": "task",
              "id": "run:ap-42/task:t1",
              "label": "wire resolver into first-spawn",
              "status": "active",
              "children": [
                {
                  "type": "worker",
                  "id": "session:w-9",
                  "label": "worker-9",
                  "status": "active",
                  "session_id": "w-9",
                  "detail": {
                    "kind": "agent",
                    "slot": "worker"
                  }
                }
              ]
            }
          ]
        },
        {
          "type": "pipeline",
          "id": "pipeline:cred-inject",
          "label": "cred-inject",
          "status": "active",
          "detail": {
            "repo": "/home/u/dev/warden"
          },
          "children": [
            {
              "type": "job",
              "id": "pipeline:cred-inject/job:implement",
              "label": "implement",
              "status": "active",
              "session_id": "impl-1",
              "detail": {
                "depends_on": []
              }
            },
            {
              "type": "job",
              "id": "pipeline:cred-inject/job:review",
              "label": "review",
              "status": "blocked",
              "detail": {
                "depends_on": [
                  "implement"
                ]
              }
            }
          ]
        },
        {
          "type": "agent",
          "id": "session:agent-7",
          "label": "orch-warden",
          "status": "waiting",
          "session_id": "agent-7",
          "detail": {
            "kind": "agent",
            "backend": "claude"
          },
          "children": [
            {
              "type": "agent",
              "id": "session:agent-8",
              "label": "sub-explorer",
              "status": "active",
              "session_id": "agent-8",
              "detail": {
                "kind": "agent",
                "backend": "claude"
              }
            }
          ]
        },
        {
          "type": "terminal",
          "id": "session:term-3",
          "label": "warden ~ main",
          "status": "idle",
          "session_id": "term-3",
          "detail": {
            "kind": "terminal"
          }
        }
      ]
    },
    {
      "type": "project",
      "id": "project:__none__",
      "label": "No project",
      "status": "idle",
      "detail": {
        "synthetic": true
      },
      "children": [
        {
          "type": "terminal",
          "id": "session:term-9",
          "label": "shell",
          "status": "idle",
          "session_id": "term-9",
          "detail": {
            "kind": "terminal"
          }
        }
      ]
    }
  ]
}`

	require.JSONEq(t, expectedJSON, string(gotJSON))
}

// Golden test: Empty fleet returns empty roots array, never null
func TestGolden_Empty(t *testing.T) {
	svc := NewService()
	tree := svc.Build(Inputs{}, "")

	require.NotNil(t, tree)
	require.Empty(t, tree.Roots)
	require.False(t, tree.Degraded)
	require.False(t, tree.Truncated)

	data, err := json.Marshal(tree)
	require.NoError(t, err)
	require.JSONEq(t, `{"roots":[]}`, string(data))
}

// Golden test: Unknown project_id returns empty roots array (not 404)
func TestGolden_UnknownProjectID_EmptyRoots(t *testing.T) {
	svc := NewService()
	in := Inputs{
		Projects: []projectstore.Project{
			{
				ID:     "/home/u/dev/warden",
				Name:   "warden",
				Path:   "/home/u/dev/warden",
				Status: projectstore.StatusOpen,
			},
		},
		Sessions: []*store.Session{
			{
				ID:        "term-1",
				Name:      "shell",
				Status:    store.StatusIdle,
				Kind:      store.KindTerminal,
				ProjectID: "/home/u/dev/warden",
			},
		},
	}

	tree := svc.Build(in, "nonexistent-project-id")
	require.NotNil(t, tree)
	require.Empty(t, tree.Roots)

	data, err := json.Marshal(tree)
	require.NoError(t, err)
	require.JSONEq(t, `{"roots":[]}`, string(data))

	// Known projectID scopes to only that project (synthetic bucket excluded)
	treeKnown := svc.Build(in, "/home/u/dev/warden")
	require.NotNil(t, treeKnown)
	require.Len(t, treeKnown.Roots, 1)
	require.Equal(t, "project:/home/u/dev/warden", treeKnown.Roots[0].ID)
}

// Golden test: Closed projects kept and marked
func TestGolden_ClosedProject(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{
			{
				ID:     "/home/u/open-proj",
				Name:   "open-proj",
				Path:   "/home/u/open-proj",
				Status: projectstore.StatusOpen,
			},
			{
				ID:     "/home/u/closed-proj",
				Name:   "closed-proj",
				Path:   "/home/u/closed-proj",
				Status: projectstore.StatusClosed,
			},
		},
		Sessions: []*store.Session{
			{
				ID:        "agent-in-closed",
				Name:      "hibernated-worker",
				ProjectID: "/home/u/closed-proj",
				Repo:      "/home/u/closed-proj",
				Status:    store.StatusDone,
				Kind:      store.KindAgent,
				CreatedAt: now,
			},
		},
	}

	svc := NewService()
	tree := svc.Build(in, "")

	require.Len(t, tree.Roots, 2)
	// Open projects sort first, then closed projects (spec §8)
	require.Equal(t, "project:/home/u/open-proj", tree.Roots[0].ID)
	require.False(t, tree.Roots[0].Detail.Closed)

	closedNode := tree.Roots[1]
	require.Equal(t, "project:/home/u/closed-proj", closedNode.ID)
	require.True(t, closedNode.Detail.Closed, "closed project must carry Detail.Closed=true")
	require.Len(t, closedNode.Children, 1)
	require.Equal(t, "session:agent-in-closed", closedNode.Children[0].ID)
}

// Golden test: Autopilot worker with cleared parent_id nested under its task
func TestGolden_AutopilotWorkerClearedParentID(t *testing.T) {
	now := time.Now()
	in := Inputs{
		Projects: []projectstore.Project{
			{
				ID:     "/home/u/dev/warden",
				Name:   "warden",
				Path:   "/home/u/dev/warden",
				Status: projectstore.StatusOpen,
			},
		},
		Autopilot: autopilot.Status{
			Enabled: true,
			Runs: []autopilot.RunStatus{
				{
					RunID: "ap-99",
					Name:  "audit-sweep",
					Repo:  "/home/u/dev/warden",
					State: autopilot.StateActive,
					PlanTasks: []autopilot.PlanTask{
						{
							ID:     "task-cleanup",
							Prompt: "Clean up unused caches",
						},
					},
					LedgerTasks: []autopilot.LedgerTask{
						{
							ID: "task-cleanup",
						},
					},
				},
			},
		},
		Sessions: []*store.Session{
			{
				ID:              "w-clean",
				Name:            "worker-clean",
				AutopilotRunID:  "ap-99",
				AutopilotSlot:   store.AutopilotSlotWorker,
				AutopilotTaskID: "task-cleanup",
				ParentID:        "", // Cleared by ownership guard!
				ProjectID:       "/home/u/dev/warden",
				Repo:            "/home/u/dev/warden",
				Status:          store.StatusWorking,
				Kind:            store.KindAgent,
				CreatedAt:       now,
			},
		},
	}

	svc := NewService()
	tree := svc.Build(in, "")

	require.Len(t, tree.Roots, 1)
	proj := tree.Roots[0]
	// Project has exactly 1 child (the autopilot run)
	require.Len(t, proj.Children, 1)
	run := proj.Children[0]
	require.Equal(t, "run:ap-99", run.ID)

	// Run has 1 child (the task)
	require.Len(t, run.Children, 1)
	task := run.Children[0]
	require.Equal(t, "run:ap-99/task:task-cleanup", task.ID)

	// Worker is nested under task
	require.Len(t, task.Children, 1)
	worker := task.Children[0]
	require.Equal(t, "session:w-clean", worker.ID)
	require.Equal(t, NodeTypeWorker, worker.Type)
	require.Equal(t, "worker", worker.Detail.Slot)
}

// Test per-subtree degradation marking (spec §12)
func TestPerSubtreeDegraded(t *testing.T) {
	in := Inputs{
		Projects: []projectstore.Project{
			{
				ID:     "/home/u/dev/warden",
				Name:   "warden",
				Path:   "/home/u/dev/warden",
				Status: projectstore.StatusOpen,
			},
		},
		Pipelines: []*pipeline.Pipeline{
			{
				ID:     "pipe-1",
				Name:   "pipe-1",
				Repo:   "/home/u/dev/warden",
				Status: pipeline.StatusRunning,
				Jobs: []pipeline.Job{
					{ID: "j1", Status: pipeline.JobRunning},
				},
			},
		},
		DegradedSubtrees: []string{"pipeline:pipe-1"},
	}

	svc := NewService()
	tree := svc.Build(in, "")

	require.True(t, tree.Degraded, "whole-tree degraded must be true when a subtree is degraded")
	require.Len(t, tree.Roots, 1)
	require.Len(t, tree.Roots[0].Children, 1)

	pipeNode := tree.Roots[0].Children[0]
	require.Equal(t, "pipeline:pipe-1", pipeNode.ID)
	require.True(t, pipeNode.Detail.Degraded)
	require.Equal(t, StatusUnknown, pipeNode.Status)
	require.Nil(t, pipeNode.Children)
}

// Test subsystem error flags
func TestSubsystemDegraded(t *testing.T) {
	in := Inputs{
		Projects: []projectstore.Project{
			{
				ID:     "/home/u/dev/warden",
				Name:   "warden",
				Path:   "/home/u/dev/warden",
				Status: projectstore.StatusOpen,
			},
		},
		Pipelines: []*pipeline.Pipeline{
			{
				ID:     "pipe-1",
				Name:   "pipe-1",
				Repo:   "/home/u/dev/warden",
				Status: pipeline.StatusRunning,
			},
		},
		PipelinesDegraded: true,
	}

	svc := NewService()
	tree := svc.Build(in, "")

	require.True(t, tree.Degraded)
	require.True(t, tree.Roots[0].Children[0].Detail.Degraded)
	require.Equal(t, StatusUnknown, tree.Roots[0].Children[0].Status)
}

// Test topological sort for jobs in a pipeline
func TestJobTopologicalSorting(t *testing.T) {
	p := &pipeline.Pipeline{
		ID:     "dag-pipeline",
		Name:   "dag-pipeline",
		Repo:   "/repo",
		Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "deploy", DependsOn: []string{"test", "build"}},
			{ID: "build", DependsOn: []string{}},
			{ID: "test", DependsOn: []string{"build"}},
		},
	}

	in := Inputs{
		Pipelines: []*pipeline.Pipeline{p},
	}

	svc := NewService()
	tree := svc.Build(in, "")

	require.Len(t, tree.Roots, 1)
	pipe := tree.Roots[0].Children[0]
	require.Len(t, pipe.Children, 3)

	// Order must be build -> test -> deploy
	require.Equal(t, "pipeline:dag-pipeline/job:build", pipe.Children[0].ID)
	require.Equal(t, "pipeline:dag-pipeline/job:test", pipe.Children[1].ID)
	require.Equal(t, "pipeline:dag-pipeline/job:deploy", pipe.Children[2].ID)
}

// Test status rollups and enum mappings
func TestStatusRollups(t *testing.T) {
	// Empty container is idle
	require.Equal(t, StatusIdle, rollup([]string{}))

	// Error wins over all
	require.Equal(t, StatusError, rollup([]string{StatusActive, StatusWaiting, StatusError, StatusDone}))

	// Active wins over waiting
	require.Equal(t, StatusActive, rollup([]string{StatusActive, StatusWaiting}))

	// Waiting when only waiting and idle/done
	require.Equal(t, StatusWaiting, rollup([]string{StatusWaiting, StatusDone, StatusIdle}))

	// Done only if all done
	require.Equal(t, StatusDone, rollup([]string{StatusDone, StatusDone}))
	require.Equal(t, StatusIdle, rollup([]string{StatusDone, StatusIdle}))

	// Task with no workers is blocked
	require.Equal(t, StatusBlocked, taskStatus([]*Node{}))
	// Task with worker rolls up worker status
	require.Equal(t, StatusActive, taskStatus([]*Node{{Status: StatusActive}}))
}
