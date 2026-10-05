package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// archiveRepo is a repo with an origin whose main is pushed, plus a branch
// with an unmerged commit (feat/keep) and one level with main (feat/gone).
func archiveRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	require.NoError(t, exec.Command("git", "init", "--bare", "-b", "main", origin).Run())
	gitRun(t, root, "init", "-b", "main")
	gitRun(t, root, "config", "user.email", "t@t.com")
	gitRun(t, root, "config", "user.name", "T")
	gitRun(t, root, "commit", "--allow-empty", "-m", "init")
	gitRun(t, root, "remote", "add", "origin", origin)
	gitRun(t, root, "push", "-u", "origin", "main")
	gitRun(t, root, "branch", "feat/gone")
	gitRun(t, root, "checkout", "-b", "feat/keep")
	gitRun(t, root, "commit", "--allow-empty", "-m", "unmerged work")
	gitRun(t, root, "checkout", "main")
	return root
}

func archiveServer(t *testing.T, root string, status store.Status) (*Server, *fakeLife, *fakeStore, *planstore.Plan) {
	t.Helper()
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	fs := newFakeStore()
	life := &fakeLife{}
	id := planstore.PlanID(root, "arch")
	agent := &agentstore.Agent{
		ID: "agent-arch", PlanID: id, Name: "M:arch", Status: status, TmuxSession: "tmux-arch",
		Repo: root, Worktree: ".worktrees/arch", Branch: "feat/keep", BranchCreated: true,
	}
	fs.data[agent.ID] = agent
	p := seedFinalizeReadyPlan(t, plans, root, "arch", agent.ID)
	require.NoError(t, plans.Update(context.Background(), p.ID, func(pl *planstore.Plan) error {
		pl.Branches = []string{"feat/keep", "feat/gone"}
		return nil
	}))
	p, err = plans.Get(context.Background(), p.ID)
	require.NoError(t, err)
	return &Server{store: fs, life: life, plans: plans, projects: projects}, life, fs, p
}

func TestArchivePlan_RefusedWithLiveExecutor(t *testing.T) {
	root := archiveRepo(t)
	srv, _, fs, p := archiveServer(t, root, store.StatusWorking)

	_, err := srv.ArchivePlan(context.Background(), oapi.ArchivePlanRequestObject{PlanId: p.ID})
	require.Error(t, err)
	require.Contains(t, err.Error(), "still running")
	require.Contains(t, err.Error(), "wd plan stop "+p.ID)

	got, err := srv.plans.Get(context.Background(), p.ID)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, got.Status)
	_, err = fs.Get(context.Background(), "agent-arch")
	require.NoError(t, err, "nothing may be torn down on refusal")
}

func TestArchivePlan_StoppedPlanTearsDownAndKeepsUnmergedBranches(t *testing.T) {
	root := archiveRepo(t)
	srv, life, fs, p := archiveServer(t, root, store.StatusDone)

	resp, err := srv.ArchivePlan(context.Background(), oapi.ArchivePlanRequestObject{PlanId: p.ID})
	require.NoError(t, err)
	out := resp.(oapi.ArchivePlan200JSONResponse)
	require.Equal(t, oapi.PlanStatusArchived, out.Status)
	require.Equal(t, oapi.PlanArchivedFromInProgress, out.ArchivedFrom)
	require.NotNil(t, out.ArchiveReport)
	require.Contains(t, out.ArchiveReport.RemovedAgents, "agent-arch")
	require.Contains(t, out.ArchiveReport.RemovedBranches, "feat/gone")
	require.Len(t, out.ArchiveReport.KeptBranches, 1)
	require.Equal(t, "feat/keep", out.ArchiveReport.KeptBranches[0].Branch)
	require.Equal(t, 1, out.ArchiveReport.KeptBranches[0].Commits)

	// The agent is gone, its worktree removal was asked not to delete the branch.
	_, err = fs.Get(context.Background(), "agent-arch")
	require.ErrorIs(t, err, agentstore.ErrNotFound)
	require.NotEmpty(t, life.removedWT)

	branches := gitRun(t, root, "branch", "--list")
	require.Contains(t, branches, "feat/keep")
	require.NotContains(t, branches, "feat/gone")
}

func TestArchiveUnarchive_FromEachOrigin(t *testing.T) {
	cases := []struct {
		name   string
		status planstore.PlanStatus
		legacy bool
		done   bool // legacy: completed_at set
		want   planstore.PlanStatus
	}{
		{"pending", planstore.PlanStatusPending, false, false, planstore.PlanStatusPending},
		{"in_progress", planstore.PlanStatusInProgress, false, false, planstore.PlanStatusInProgress},
		{"completed", planstore.PlanStatusCompleted, false, true, planstore.PlanStatusCompleted},
		{"legacy completed", planstore.PlanStatusCompleted, true, true, planstore.PlanStatusCompleted},
		{"legacy pending", planstore.PlanStatusPending, true, false, planstore.PlanStatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := archiveRepo(t)
			srv, _, _, p := archiveServer(t, root, store.StatusDone)
			ctx := context.Background()
			require.NoError(t, srv.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
				pl.Status = tc.status
				return nil
			}))
			if tc.status == planstore.PlanStatusPending {
				require.NoError(t, srv.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
					pl.ActiveExecution, pl.OrchestratorID = nil, ""
					return nil
				}))
			}
			if tc.done {
				require.NoError(t, srv.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
					now := pl.CreatedAt
					pl.CompletedAt = &now
					return nil
				}))
			}
			_, err := srv.ArchivePlan(ctx, oapi.ArchivePlanRequestObject{PlanId: p.ID})
			require.NoError(t, err)
			if tc.legacy {
				require.NoError(t, srv.plans.Update(ctx, p.ID, func(pl *planstore.Plan) error {
					pl.ArchivedFrom = ""
					return nil
				}))
			}
			resp, err := srv.UnarchivePlan(ctx, oapi.UnarchivePlanRequestObject{PlanId: p.ID})
			require.NoError(t, err)
			got := resp.(oapi.UnarchivePlan200JSONResponse)
			require.Equal(t, oapi.PlanStatus(tc.want), got.Status)
			require.True(t, got.ArchivedAt.IsZero())
			require.Empty(t, got.ArchivedFrom)

			// Not archived any more: a second unarchive is refused.
			_, err = srv.UnarchivePlan(ctx, oapi.UnarchivePlanRequestObject{PlanId: p.ID})
			require.Error(t, err)
			require.Contains(t, err.Error(), "not archived")
		})
	}
}
