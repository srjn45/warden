package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

// upgradeBootResult captures the stores opened by one simulated daemon start.
type upgradeBootResult struct {
	Agents   *agentstore.Store
	Terms    *terminalstore.Store
	Plans    *planstore.Store
	Projects *projectstore.Store
	Pipes    *pipeline.Store
	LiveAP   *autopilotstore.Store
	APRep    autopilotstore.MigrateReport
	MemRep   MembershipReconcileReport
}

func (b *upgradeBootResult) Close() {
	if b == nil {
		return
	}
	if b.Agents != nil {
		_ = b.Agents.Close()
	}
	if b.Terms != nil {
		_ = b.Terms.Close()
	}
	if b.Plans != nil {
		_ = b.Plans.Close()
	}
	if b.Projects != nil {
		_ = b.Projects.Close()
	}
	if b.Pipes != nil {
		_ = b.Pipes.Close()
	}
	if b.LiveAP != nil {
		_ = b.LiveAP.Close()
	}
}

// simulateDaemonBoot mirrors the ordered migrations the real daemon runs at
// start: agentstore + terminalstore import, project membership reconcile, then
// autopilot legacy-run migration. Closing and reopening models a second start.
func simulateDaemonBoot(t *testing.T, dataDir, plansDir, projectsDir, pipesDir string) *upgradeBootResult {
	t.Helper()
	ctx := context.Background()
	out := &upgradeBootResult{}

	require.NoError(t, agentstore.LegacyImport.Import(dataDir))
	require.NoError(t, terminalstore.LegacyImport.Import(dataDir))

	agents, err := agentstore.New(dataDir)
	require.NoError(t, err)
	out.Agents = agents

	terms, err := terminalstore.New(dataDir)
	require.NoError(t, err)
	out.Terms = terms

	plans, err := planstore.New(plansDir)
	require.NoError(t, err)
	out.Plans = plans

	projects, err := projectstore.NewStore(projectsDir)
	require.NoError(t, err)
	out.Projects = projects

	pipes, err := pipeline.NewStore(pipesDir)
	require.NoError(t, err)
	out.Pipes = pipes

	memRep, err := ReconcileProjectMembership(ctx, agents, pipes, plans, projects)
	require.NoError(t, err)
	out.MemRep = memRep

	live, err := autopilotstore.New(dataDir)
	require.NoError(t, err)
	out.LiveAP = live

	apRep, err := autopilotstore.MigrateLegacyRuns(ctx, dataDir, live, plans)
	require.NoError(t, err)
	out.APRep = apRep

	return out
}

func seedUpgradeLegacyRun(t *testing.T, dataDir string, run autopilotstore.LegacyRunRecord) {
	t.Helper()
	dir := filepath.Join(dataDir, "autopilot", "runs-db")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	require.NoError(t, err)
	defer db.Close()
	col, err := db.Collection("autopilot_runs")
	require.NoError(t, err)
	b, err := json.Marshal(run)
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(b, &rec))
	_, _, err = col.InsertWithKey(run.RunID, rec)
	require.NoError(t, err)
}

// TestUpgradeAcceptanceFromLegacyCorpus is the plan-execution-entity-redesign
// upgrade gate: seed representative pre-redesign data, boot twice, then prove
// attachability, planless spawn, mode prefixes, Autopilot PlanID enforcement,
// and durable ExecutionSummary after executor cleanup.
func TestUpgradeAcceptanceFromLegacyCorpus(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	plansDir := t.TempDir()
	projectsDir := t.TempDir()
	pipesDir := t.TempDir()
	repo := t.TempDir()
	gitInit(t, repo)

	// --- Seed representative legacy corpus ---------------------------------
	legacy, err := store.NewFileStore(dataDir)
	require.NoError(t, err)

	liveAgent := &store.Session{
		ID:             "agent-live",
		Name:           "legacy-worker",
		Status:         store.StatusWorking,
		Kind:           store.KindAgent,
		Backend:        "claude", // deprecated field name on wire
		AICLISessionID: "resume-legacy-1",
		TmuxSession:    "agent-live",
		Workdir:        filepath.Join(repo, "wt-live"),
		ProjectID:      repo,
		Tags:           []string{"upgrade"},
	}
	require.NoError(t, legacy.Insert(ctx, liveAgent))

	termSess := &store.Session{
		ID:          "term-shell",
		Name:        "dev-shell",
		Kind:        store.KindTerminal,
		Status:      store.StatusIdle,
		TmuxSession: "term-shell",
		Workdir:     repo,
		ProjectID:   repo,
	}
	require.NoError(t, legacy.Insert(ctx, termSess))

	archived := &store.Session{
		ID:             "agent-archived",
		Name:           "old-worker",
		Status:         store.StatusOrphaned,
		AICLISessionID: "resume-archived",
		TmuxSession:    "agent-archived",
		Workdir:        filepath.Join(repo, "wt-arch"),
		ProjectID:      repo,
	}
	require.NoError(t, legacy.Insert(ctx, archived))
	require.NoError(t, legacy.Archive(ctx, "agent-archived"))
	require.NoError(t, legacy.Close(ctx))

	projects, err := projectstore.NewStore(projectsDir)
	require.NoError(t, err)
	// Seed without OpenProject: that helper writes Plans=[]string{}, and a later
	// Upsert(Plans:nil) preserves the empty slice. Reconcile only backfills when
	// Plans is truly nil (never written as empty first).
	require.NoError(t, projects.Upsert(projectstore.Project{
		ID:        repo,
		Name:      "upgrade-repo",
		Path:      repo,
		Status:    projectstore.StatusOpen,
		Agents:    []string{"agent-live", "dangling-agent"},
		Terminals: []string{"term-shell"},
		// Plans intentionally omitted (nil) so membership reconcile backfills.
	}))
	require.NoError(t, projects.Close())

	seedPlanYAML(t, repo, "plans/in_progress/legacy-ap.yaml", "legacy-ap")
	planID := planstore.PlanID(repo, "legacy-ap")
	plansSeed, err := planstore.New(plansDir)
	require.NoError(t, err)
	require.NoError(t, plansSeed.Create(ctx, &planstore.Plan{
		ID: planID, ProjectID: repo, Name: "legacy-ap",
		FilePath: "plans/in_progress/legacy-ap.yaml",
		Status:   planstore.PlanStatusInProgress,
	}))
	require.NoError(t, plansSeed.Close())

	now := time.Now().UTC().Truncate(time.Millisecond)
	seedUpgradeLegacyRun(t, dataDir, autopilotstore.LegacyRunRecord{
		RunID:             "ap-legacyaccept01",
		Name:              "legacy-ap",
		Repo:              repo,
		PlanFile:          filepath.Join(repo, "plans/in_progress/legacy-ap.yaml"),
		PlanID:            planID,
		ProjectID:         repo,
		State:             "active",
		IntegrationBranch: "autopilot/legacy-ap",
		Gate:              "local",
		BrainID:           "legacy-ap-autopilot",
		SlotScope:         "legacy-ap",
		CreatedAt:         now,
		UpdatedAt:         now,
	})

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("backend_default: codex\n"), 0o600))
	cfg := config.Load(cfgPath)
	require.Equal(t, "codex", cfg.GetAiCliDefault(), "old backend_default must populate ai_cli_default")

	// --- Boot #1 -----------------------------------------------------------
	boot1 := simulateDaemonBoot(t, dataDir, plansDir, projectsDir, pipesDir)
	require.Equal(t, 1, boot1.APRep.LiveCreated)
	require.True(t, boot1.APRep.Changed())

	agents1, err := boot1.Agents.List(ctx)
	require.NoError(t, err)
	require.Len(t, agents1, 1)
	require.Equal(t, "agent-live", agents1[0].ID)
	require.Equal(t, "claude", agents1[0].AiCli)
	require.Equal(t, "resume-legacy-1", agents1[0].AICLISessionID)

	closed, err := boot1.Agents.ListClosed(ctx)
	require.NoError(t, err)
	require.Len(t, closed, 1)
	require.Equal(t, "agent-archived", closed[0].ID)

	terms1, err := boot1.Terms.List(ctx)
	require.NoError(t, err)
	require.Len(t, terms1, 1)
	require.Equal(t, "term-shell", terms1[0].ID)

	ap1, err := boot1.LiveAP.Get(ctx, "ap-legacyaccept01")
	require.NoError(t, err)
	require.Equal(t, planID, ap1.PlanID)
	require.Equal(t, "AP:legacy-ap", ap1.Name)

	proj1, err := boot1.Projects.Get(repo)
	require.NoError(t, err)
	require.Contains(t, proj1.Agents, "agent-live")
	require.Contains(t, proj1.Terminals, "term-shell")
	require.Contains(t, proj1.Plans, planID)

	boot1.Close()

	// --- Boot #2 (idempotent) ----------------------------------------------
	boot2 := simulateDaemonBoot(t, dataDir, plansDir, projectsDir, pipesDir)
	t.Cleanup(boot2.Close)
	require.False(t, boot2.APRep.Changed(), "second boot must not re-create Autopilots")
	require.False(t, boot2.MemRep.Changed(), "second membership reconcile must be idempotent")

	agents2, err := boot2.Agents.List(ctx)
	require.NoError(t, err)
	require.Len(t, agents2, 1)
	terms2, err := boot2.Terms.List(ctx)
	require.NoError(t, err)
	require.Len(t, terms2, 1)
	aps, err := boot2.LiveAP.List(ctx)
	require.NoError(t, err)
	require.Len(t, aps, 1)

	// --- Attachable after upgrade ------------------------------------------
	life := &fakeLife{}
	srv := NewServer(boot2.Agents, life, nil, time.Second, true, nil, nil, nil)
	srv.SetTerminals(boot2.Terms)
	srv.plans = boot2.Plans
	srv.projects = boot2.Projects

	attachedAgent, err := srv.resolveSessionDTO(ctx, "legacy-worker")
	require.NoError(t, err)
	require.Equal(t, "agent-live", attachedAgent.TmuxSession)
	require.Equal(t, "resume-legacy-1", attachedAgent.AICLISessionID)

	attachedTerm, err := srv.resolveSessionDTO(ctx, "term-shell")
	require.NoError(t, err)
	require.Equal(t, store.KindTerminal, attachedTerm.Kind)
	require.Equal(t, "term-shell", attachedTerm.TmuxSession)

	history, err := srv.ListHistory(ctx, oapi.ListHistoryRequestObject{})
	require.NoError(t, err)
	rows := history.(oapi.ListHistory200JSONResponse).Sessions
	require.Len(t, rows, 1)
	require.Equal(t, "agent-archived", rows[0].ID)

	// --- Independent Agents / Pipelines still spawn ------------------------
	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	exec := NewExecutor(boot2.Pipes, boot2.Agents, life, cs, func() {})
	srv.exec = exec
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	spawnBody, _ := json.Marshal(map[string]any{
		"role": "general", "prompt": "independent analysis", "name": "test-agent",
		"project_id": repo, "cwd": repo,
	})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(spawnBody))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, life.spawned)
	require.Empty(t, life.spawned.PlanID)
	// fakeLife.Spawn uses a fixed free-form id ("agent-test"); free it so later
	// plan-bound spawns in this suite can persist without colliding.
	require.NoError(t, boot2.Agents.Delete(ctx, life.spawned.ID))

	createdPipe := createPipeline(t, ts.URL, `{"project_id":"`+repo+`","spec":"name: independent\nrepo: `+repo+`\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`)
	require.Empty(t, createdPipe.PlanID)
	require.Equal(t, "independent", createdPipe.Name)

	// --- Every plan mode creates the required prefix -----------------------
	runs, err := autopilot.NewRunStore(dataDir)
	require.NoError(t, err)
	controller := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: repo, RunStore: runs, LiveStore: boot2.LiveAP, PlanSource: boot2.Plans, Gate: "local",
	}, autopilot.NewExecEnv())
	t.Cleanup(func() { require.NoError(t, controller.Close()) })
	srv.autopilot = controller

	t.Run("mode_prefixes", func(t *testing.T) {
		type modeCase struct {
			mode  string
			name  string
			check func(t *testing.T, plan planstore.Plan, spawned *agentstore.Agent)
		}
		cases := []modeCase{
			{
				mode: "manual", name: "acc-manual",
				check: func(t *testing.T, _ planstore.Plan, spawned *agentstore.Agent) {
					require.NotNil(t, spawned)
					require.Equal(t, "M:acc-manual", spawned.Name)
				},
			},
			{
				mode: "orchestrator_worker", name: "acc-orch",
				check: func(t *testing.T, p planstore.Plan, spawned *agentstore.Agent) {
					require.NotNil(t, spawned)
					require.Equal(t, "O:acc-orch", spawned.Name)
					require.NotEmpty(t, p.OrchestratorID)
				},
			},
			{
				mode: "pipeline", name: "acc-pipe",
				check: func(t *testing.T, p planstore.Plan, _ *agentstore.Agent) {
					pl, err := boot2.Pipes.Get(p.PipelineID)
					require.NoError(t, err)
					require.Equal(t, "P:acc-pipe", pl.Name)
					require.Equal(t, p.ID, pl.PlanID)
				},
			},
			{
				mode: "autopilot", name: "acc-ap",
				check: func(t *testing.T, p planstore.Plan, _ *agentstore.Agent) {
					ap, err := boot2.LiveAP.Get(ctx, p.AutopilotRunID)
					require.NoError(t, err)
					require.Equal(t, "AP:acc-ap", ap.Name)
					require.Equal(t, p.ID, ap.PlanID)
				},
			},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(tc.mode, func(t *testing.T) {
				life.spawned = nil
				// Clear the fixed fakeLife free-form id between modes.
				_ = boot2.Agents.Delete(ctx, "agent-test")
				seedPlanYAML(t, repo, "plans/pending/"+tc.name+".yaml", tc.name)
				id := planstore.PlanID(repo, tc.name)
				require.NoError(t, boot2.Plans.Create(ctx, &planstore.Plan{
					ID: id, ProjectID: repo, Name: tc.name,
					FilePath: "plans/pending/" + tc.name + ".yaml",
					Status:   planstore.PlanStatusPending,
				}))
				r := postJSON(t, planURL(ts.URL, repo, "/"+id+"/run"), map[string]any{"mode": tc.mode})
				defer r.Body.Close()
				body, _ := io.ReadAll(r.Body)
				require.Equal(t, http.StatusOK, r.StatusCode, string(body))
				var got planstore.Plan
				require.NoError(t, json.Unmarshal(body, &got))
				tc.check(t, got, life.spawned)
			})
		}
	})

	// --- Autopilot rejects no-PlanID creation ------------------------------
	err = boot2.LiveAP.Create(ctx, &autopilotstore.Autopilot{
		ID: "ap-no-plan", ProjectID: repo, Name: "AP:ghost",
	})
	require.ErrorIs(t, err, autopilotstore.ErrPlanRequired)

	// --- Completed plan retains deterministic summary after executor cleanup
	t.Run("finalize_keeps_summary", func(t *testing.T) {
		agentID := "agent-fin-acc"
		p := seedFinalizeReadyPlan(t, boot2.Plans, repo, "acc-fin", agentID)
		finAgent := &agentstore.Agent{
			ID: agentID, PlanID: p.ID, Name: "M:acc-fin",
			Status: store.StatusWorking, TmuxSession: "tmux-acc-fin",
			Worktree: repo + "/.worktrees/acc-fin", Branch: "feat/acc-fin", BranchCreated: true,
		}
		require.NoError(t, boot2.Agents.Insert(ctx, finAgent))

		finLife := &fakeLife{}
		finSrv := &Server{store: boot2.Agents, life: finLife, plans: boot2.Plans, projects: boot2.Projects}
		var sawSummaryBeforeDelete bool
		finSrv.finalizeCleanupHook = func(ctx context.Context, pl *planstore.Plan) planstore.CleanupEvidence {
			require.NotNil(t, pl.ExecutionSummary)
			sawSummaryBeforeDelete = true
			return finSrv.cleanupPlanExecutors(ctx, pl)
		}

		res, err := finSrv.FinalizePlan(ctx, p.ID)
		require.NoError(t, err)
		require.True(t, sawSummaryBeforeDelete)
		require.Equal(t, planstore.PlanStatusCompleted, res.Plan.Status)
		require.NotNil(t, res.Plan.ExecutionSummary)

		summary1 := *res.Plan.ExecutionSummary
		// Deterministic: reducing the same events again yields the same summary.
		events, err := boot2.Plans.ListEvents(ctx, p.ID, "pe-fin01")
		require.NoError(t, err)
		reduced := planstore.ReduceEvents(events)
		require.Equal(t, summary1.PlanID, reduced.PlanID)
		require.Equal(t, summary1.PlanName, reduced.PlanName)
		require.Equal(t, summary1.ExecutorID, reduced.ExecutorID)
		require.Equal(t, summary1.TasksTotal, reduced.TasksTotal)

		_, err = boot2.Agents.Get(ctx, agentID)
		require.ErrorIs(t, err, agentstore.ErrNotFound, "executor must be removed")

		still, err := boot2.Plans.Get(ctx, p.ID)
		require.NoError(t, err)
		require.NotNil(t, still.ExecutionSummary)
		require.Equal(t, summary1, *still.ExecutionSummary, "summary must survive executor deletion")
	})
}
