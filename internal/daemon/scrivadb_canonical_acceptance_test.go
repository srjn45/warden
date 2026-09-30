package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planbackup"
	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/plansync"
)

// TestScrivaDBCanonicalPlans_Phase12Acceptance is the Phase 12 upgrade and
// acceptance gate for docs/specs/2026-09-30-scrivadb-canonical-plans.md.
//
// It seeds one representative corpus (legacy YAML in every lifecycle state,
// existing canonical Plans + execution events, an unexported Plan, a stale
// replica, duplicate-import targets, and a project with unrelated dirty files)
// and proves the cutover contracts end-to-end against real ScrivaDB stores plus
// the HTTP API surface.
func TestScrivaDBCanonicalPlans_Phase12Acceptance(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, 9, 30, 16, 0, 0, 0, time.UTC)

	repo := t.TempDir()
	plansDir := t.TempDir()
	ps, err := planstore.New(plansDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	exports, err := planexport.NewStore(plansDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = exports.Close() })

	svc := planstore.NewPlanService(ps,
		planstore.WithProjectRoot(func(string) string { return repo }),
		planstore.WithNow(func() time.Time { return fixed }),
	)

	projectID := repo

	// --- Seed: legacy YAML in every lifecycle directory --------------------
	writeLegacyYAML(t, repo, "plans/pending/legacy-pending.yaml",
		"version: 1\nname: legacy-pending\ngoal: pending goal\ntasks:\n  - id: t1\n    prompt: pending work\n")
	writeLegacyYAML(t, repo, "plans/in_progress/legacy-wip.yaml",
		"version: 1\nname: legacy-wip\ngoal: wip goal\ntasks:\n  - id: a\n    prompt: first\n  - id: b\n    prompt: second\n    after: [a]\n")
	writeLegacyYAML(t, repo, "plans/completed/legacy-done.yaml",
		"version: 1\nname: legacy-done\ngoal: done goal\ntasks:\n  - id: t1\n    prompt: finished\n    status: done\n")
	writeLegacyYAML(t, repo, "plans/archived/legacy-old.yaml",
		"version: 1\nname: legacy-old\ngoal: archived goal\ntasks:\n  - id: t1\n    prompt: old work\n")

	// --- Seed: existing canonical Plan with execution events ---------------
	canon := &planstore.Plan{
		ID:        planstore.PlanID(projectID, "already-canonical"),
		ProjectID: projectID,
		Name:      "already-canonical",
		Goal:      "canonical goal",
		Status:    planstore.PlanStatusInProgress,
		Revision:  2,
		Tasks: []planstore.PlanTask{
			{ID: "design", Prompt: "design it"},
			{ID: "build", Prompt: "build it", After: []string{"design"}},
		},
		TaskProgress:  map[string]string{"design": "done", "build": "in_progress"},
		ExecutionMode: planstore.PlanModeManual,
		StartedAt:     ptrTime(fixed.Add(-2 * time.Hour)),
		ExecutionHistory: []planstore.PlanExecution{{
			ID:            "pe-canon01",
			PlanID:        planstore.PlanID(projectID, "already-canonical"),
			ExecutionMode: planstore.PlanModeManual,
			StartedAt:     fixed.Add(-2 * time.Hour),
			TaskProgress:  map[string]string{"design": "done", "build": "in_progress"},
		}},
	}
	canon.ContentHash = planstore.ComputeContentHash(canon)
	require.NoError(t, ps.Create(ctx, canon))
	require.NoError(t, ps.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		ID: "ev-canon-start", PlanID: canon.ID, ExecutionID: "pe-canon01",
		Kind: planstore.EventKindExecutionStarted, OccurredAt: fixed.Add(-2 * time.Hour),
	}))

	// Matching legacy replica for the already-canonical Plan (duplicate import target).
	writeLegacyYAML(t, repo, "plans/in_progress/already-canonical.yaml",
		"version: 1\nname: already-canonical\ngoal: canonical goal\ntasks:\n"+
			"  - id: design\n    prompt: design it\n  - id: build\n    prompt: build it\n    after: [design]\n")

	// --- Seed: unexported Plan (DB-only; no plans/ replica) ----------------
	unexported, err := svc.Create(ctx, projectID, planstore.CreateRequest{
		Name: "db-only",
		Goal: "never exported",
		Tasks: []planstore.TaskSpec{
			{ID: "solo", Prompt: "stay in ScrivaDB"},
		},
	})
	require.NoError(t, err)
	require.Empty(t, unexported.FilePath)
	require.Nil(t, unexported.RepoExport)

	// --- Seed: stale replica that disagrees with canonical -----------------
	writeLegacyYAML(t, repo, "plans/pending/already-canonical-stale.yaml",
		"version: 1\nname: already-canonical\ngoal: HIJACKED FROM DISK\ntasks:\n  - id: evil\n    prompt: should not win\n")
	// Point FilePath at the stale path as last-export metadata only.
	require.NoError(t, ps.Update(ctx, canon.ID, func(p *planstore.Plan) error {
		p.FilePath = "plans/pending/already-canonical-stale.yaml"
		return nil
	}))

	// --- Seed: unrelated dirty operator files ------------------------------
	dirtyPath := filepath.Join(repo, "operator-wip.txt")
	require.NoError(t, os.WriteFile(dirtyPath, []byte("do not commit me\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "NOTES.md"), []byte("# operator notes\n"), 0o644))

	// =====================================================================
	// Gate A — legacy import is idempotent; conflicts do not mutate
	// =====================================================================
	r1, err := svc.ImportLegacy(ctx, projectID, planstore.ImportOptions{})
	require.NoError(t, err)
	imported, skipped, conflicted, errored := r1.Counts()
	require.GreaterOrEqual(t, imported, 4, "all four lifecycle YAML should import")
	require.GreaterOrEqual(t, skipped, 1, "matching already-canonical replica must skip")
	require.Equal(t, 0, errored)
	_ = conflicted // stale differently-named file may conflict or import under another identity

	pending, err := ps.Get(ctx, planstore.PlanID(projectID, "legacy-pending"))
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusPending, pending.Status)

	wip, err := ps.Get(ctx, planstore.PlanID(projectID, "legacy-wip"))
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, wip.Status)

	done, err := ps.Get(ctx, planstore.PlanID(projectID, "legacy-done"))
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusCompleted, done.Status)

	arch, err := ps.Get(ctx, planstore.PlanID(projectID, "legacy-old"))
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusArchived, arch.Status)

	// Second import: pure no-op for hash-matching files.
	r2, err := svc.ImportLegacy(ctx, projectID, planstore.ImportOptions{})
	require.NoError(t, err)
	imported2, skipped2, _, errored2 := r2.Counts()
	require.Equal(t, 0, imported2)
	require.GreaterOrEqual(t, skipped2, 4)
	require.Equal(t, 0, errored2)

	// Mutated YAML → conflicted; canonical unchanged.
	beforeDup, err := ps.Get(ctx, planstore.PlanID(projectID, "legacy-pending"))
	require.NoError(t, err)
	writeLegacyYAML(t, repo, "plans/pending/legacy-pending.yaml",
		"version: 1\nname: legacy-pending\ngoal: CHANGED AFTER IMPORT\ntasks:\n  - id: t1\n    prompt: pending work\n")
	r3, err := svc.ImportLegacy(ctx, projectID, planstore.ImportOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(r3.Conflicted), 1)
	afterDup, err := ps.Get(ctx, planstore.PlanID(projectID, "legacy-pending"))
	require.NoError(t, err)
	require.Equal(t, beforeDup.Goal, afterDup.Goal)
	require.Equal(t, beforeDup.ContentHash, afterDup.ContentHash)
	require.Equal(t, beforeDup.Revision, afterDup.Revision)

	// =====================================================================
	// Gate B — replicas never influence listing or execution
	// =====================================================================
	gotCanon, err := svc.Get(ctx, canon.ID)
	require.NoError(t, err)
	require.Equal(t, "canonical goal", gotCanon.Goal)
	require.Equal(t, "design", gotCanon.Tasks[0].ID)
	require.NotEqual(t, "HIJACKED FROM DISK", gotCanon.Goal)

	listed, err := svc.List(ctx, projectID, planstore.PlanStatusInProgress)
	require.NoError(t, err)
	var foundCanon bool
	for _, p := range listed {
		if p.ID == canon.ID {
			foundCanon = true
			require.Equal(t, "canonical goal", p.Goal)
		}
		require.NotEqual(t, "HIJACKED FROM DISK", p.Goal)
	}
	require.True(t, foundCanon)

	// =====================================================================
	// Gate C — normal Plan CRUD + execution require no plans/ directory
	// =====================================================================
	cleanRoot := t.TempDir()
	cleanSvc := planstore.NewPlanService(ps,
		planstore.WithProjectRoot(func(string) string { return cleanRoot }),
		planstore.WithNow(func() time.Time { return fixed }),
	)
	_, err = os.Stat(filepath.Join(cleanRoot, "plans"))
	require.True(t, os.IsNotExist(err))

	native, err := cleanSvc.Create(ctx, cleanRoot, planstore.CreateRequest{
		Name: "native-flow",
		Goal: "no yaml needed",
		Tasks: []planstore.TaskSpec{
			{ID: "t1", Prompt: "one"},
			{ID: "t2", Prompt: "two", After: []string{"t1"}},
		},
	})
	require.NoError(t, err)

	native, err = cleanSvc.Transition(ctx, native.ID, planstore.PlanStatusInProgress, planstore.TransitionOptions{
		ExecutionMode: planstore.PlanModeManual,
	})
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, native.Status)
	_, err = cleanSvc.UpdateTaskStatus(ctx, native.ID, "t1", "done")
	require.NoError(t, err)
	_, err = cleanSvc.UpdateTaskStatus(ctx, native.ID, "t2", "done")
	require.NoError(t, err)
	native, err = cleanSvc.Transition(ctx, native.ID, planstore.PlanStatusCompleted, planstore.TransitionOptions{})
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusCompleted, native.Status)
	_, err = os.Stat(filepath.Join(cleanRoot, "plans"))
	require.True(t, os.IsNotExist(err), "DB-native lifecycle must never create plans/")

	// Unexported Plan remains usable without a replica.
	ue, err := svc.Get(ctx, unexported.ID)
	require.NoError(t, err)
	require.Empty(t, ue.FilePath)
	require.Equal(t, "never exported", ue.Goal)

	// =====================================================================
	// Gate D — DB revision conflicts are structured (store + HTTP API)
	// =====================================================================
	editable, err := svc.Create(ctx, projectID, planstore.CreateRequest{
		Name:  "rev-conflict",
		Goal:  "v1",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "x"}},
	})
	require.NoError(t, err)
	goal2 := "v2"
	editable, err = svc.Update(ctx, editable.ID, planstore.UpdateRequest{
		Goal: &goal2, ExpectedRevision: &editable.Revision,
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), editable.Revision)

	staleRev := int64(1)
	goalStale := "stale"
	_, err = svc.Update(ctx, editable.ID, planstore.UpdateRequest{
		Goal: &goalStale, ExpectedRevision: &staleRev,
	})
	require.ErrorIs(t, err, planstore.ErrRevisionConflict)
	var conflict *planstore.RevisionConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, editable.ID, conflict.PlanID)
	require.Equal(t, int64(1), conflict.Expected)
	require.Equal(t, int64(2), conflict.Actual)

	// HTTP surface returns PlanMutationConflict fields.
	gitFake := &acceptFakeGit{
		identity:  "github.com/example/scrivadb-accept",
		prURL:     "https://github.com/example/scrivadb-accept/pull/42",
		sha:       "abc123def456",
		repoDirty: true,
		repoPath:  repo,
	}
	srv := &Server{
		store:       newFakeStore(),
		life:        &fakeLife{},
		plans:       ps,
		planExports: exports,
		planSyncGit: gitFake,
	}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]any{
		"goal": "api-stale", "expected_revision": 1,
	})
	req, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/v1/plans/"+editable.ID, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	apiResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer apiResp.Body.Close()
	require.Equal(t, http.StatusConflict, apiResp.StatusCode)
	var apiConflict oapi.PlanMutationConflict
	require.NoError(t, json.NewDecoder(apiResp.Body).Decode(&apiConflict))
	require.Equal(t, editable.ID, apiConflict.PlanId)
	require.Equal(t, int64(1), apiConflict.Expected)
	require.Equal(t, int64(2), apiConflict.Actual)

	// =====================================================================
	// Gate E — sync_to_repo: one isolated PR per changed revision; repeat no-op
	// =====================================================================
	syncPlan, err := svc.Create(ctx, projectID, planstore.CreateRequest{
		Name:  "sync-me",
		Goal:  "export once",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "ship replica"}},
	})
	require.NoError(t, err)

	syncBody, _ := json.Marshal(map[string]any{"target_ref": "main"})
	syncResp, err := http.Post(ts.URL+"/api/v1/plans/"+syncPlan.ID+"/sync_to_repo",
		"application/json", bytes.NewReader(syncBody))
	require.NoError(t, err)
	defer syncResp.Body.Close()
	require.Equal(t, http.StatusOK, syncResp.StatusCode)
	var syncOut map[string]any
	require.NoError(t, json.NewDecoder(syncResp.Body).Decode(&syncOut))
	require.Equal(t, "success", syncOut["outcome"])
	require.Equal(t, false, syncOut["reused"])
	require.Equal(t, "https://github.com/example/scrivadb-accept/pull/42", syncOut["pr_url"])
	require.Equal(t, 1, gitFake.prCalls)
	require.True(t, gitFake.repoDirty, "fixture: operator tree stays dirty")
	require.True(t, gitFake.pushed)

	// Repeat sync → no-op (no second PR).
	syncResp2, err := http.Post(ts.URL+"/api/v1/plans/"+syncPlan.ID+"/sync_to_repo",
		"application/json", bytes.NewReader(syncBody))
	require.NoError(t, err)
	defer syncResp2.Body.Close()
	require.Equal(t, http.StatusOK, syncResp2.StatusCode)
	var syncOut2 map[string]any
	require.NoError(t, json.NewDecoder(syncResp2.Body).Decode(&syncOut2))
	require.Equal(t, "skipped", syncOut2["outcome"])
	require.Equal(t, true, syncOut2["reused"])
	require.Equal(t, 1, gitFake.prCalls, "idempotent reuse must not open another PR")

	// Bump revision → exactly one new PR for the new revision.
	newGoal := "export twice"
	syncPlan, err = svc.Update(ctx, syncPlan.ID, planstore.UpdateRequest{Goal: &newGoal})
	require.NoError(t, err)
	require.Equal(t, int64(2), syncPlan.Revision)
	gitFake.prURL = "https://github.com/example/scrivadb-accept/pull/43"
	syncResp3, err := http.Post(ts.URL+"/api/v1/plans/"+syncPlan.ID+"/sync_to_repo",
		"application/json", bytes.NewReader(syncBody))
	require.NoError(t, err)
	defer syncResp3.Body.Close()
	require.Equal(t, http.StatusOK, syncResp3.StatusCode)
	var syncOut3 map[string]any
	require.NoError(t, json.NewDecoder(syncResp3.Body).Decode(&syncOut3))
	require.Equal(t, "success", syncOut3["outcome"])
	require.Equal(t, false, syncOut3["reused"])
	require.Equal(t, float64(2), syncOut3["revision"])
	require.Equal(t, 2, gitFake.prCalls, "exactly one PR per changed revision")

	// Dirty operator files untouched on disk.
	dirtyBytes, err := os.ReadFile(dirtyPath)
	require.NoError(t, err)
	require.Equal(t, []byte("do not commit me\n"), dirtyBytes)

	// =====================================================================
	// Gate F — failed sync preserves canonical data
	// =====================================================================
	failPlan, err := svc.Create(ctx, projectID, planstore.CreateRequest{
		Name:  "fail-sync",
		Goal:  "must survive auth failure",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "keep me"}},
	})
	require.NoError(t, err)
	beforeFail, err := ps.Get(ctx, failPlan.ID)
	require.NoError(t, err)

	failGit := &acceptFakeGit{
		identity: "github.com/example/scrivadb-accept",
		authErr:  errors.New("not logged into any GitHub hosts"),
	}
	failSyncer := &planexport.Syncer{
		Plans: ps, Exports: exports, PlansMut: ps, Git: failGit, Renderer: planexport.Default(),
		Now: func() time.Time { return fixed },
	}
	failRes, err := failSyncer.Sync(ctx, planexport.SyncOptions{
		PlanID: failPlan.ID, RepoPath: repo, TargetRef: "main",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, planexport.ErrGitHubAuth)
	require.Equal(t, planexport.OutcomeFailed, failRes.Outcome)
	afterFail, err := ps.Get(ctx, failPlan.ID)
	require.NoError(t, err)
	require.Equal(t, beforeFail.Goal, afterFail.Goal)
	require.Equal(t, beforeFail.ContentHash, afterFail.ContentHash)
	require.Equal(t, beforeFail.Revision, afterFail.Revision)
	require.Nil(t, afterFail.RepoExport, "failed sync must not write RepoExport")
	require.Equal(t, 0, failGit.prCalls)

	// =====================================================================
	// Gate G — restore works on a clean data directory (no Git / no plans/)
	// =====================================================================
	exporter := &planbackup.Exporter{Store: ps, Now: func() time.Time { return fixed }}
	bundle, err := exporter.Export(ctx, planbackup.ExportOptions{PlanIDs: []string{unexported.ID, canon.ID}})
	require.NoError(t, err)
	require.Len(t, bundle.Entries, 2)

	dstDir := t.TempDir()
	dst, err := planstore.New(dstDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dst.Close() })
	_, err = os.Stat(filepath.Join(dstDir, "plans"))
	require.True(t, os.IsNotExist(err))

	restorer := &planbackup.Restorer{Store: dst}
	restoreRes, err := restorer.Restore(ctx, bundle, planbackup.RestoreOptions{})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(restoreRes.Entries), 2)

	restored, err := dst.Get(ctx, unexported.ID)
	require.NoError(t, err)
	require.Equal(t, "never exported", restored.Goal)
	require.Equal(t, unexported.ContentHash, restored.ContentHash)

	restoredCanon, err := dst.Get(ctx, canon.ID)
	require.NoError(t, err)
	require.Equal(t, "canonical goal", restoredCanon.Goal)
	require.Equal(t, planstore.PlanStatusInProgress, restoredCanon.Status)

	dstSvc := planstore.NewPlanService(dst, planstore.WithProjectRoot(func(string) string {
		return t.TempDir() // fresh root with no plans/
	}), planstore.WithNow(func() time.Time { return fixed }))
	listedRestored, err := dstSvc.List(ctx, projectID, "")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(listedRestored), 2)

	// =====================================================================
	// Gate H — no Hub transport is invoked (default provider is local/offline)
	// =====================================================================
	hub := plansync.Default()
	require.Equal(t, plansync.ProviderLocal, hub.Name())
	require.False(t, hub.Enabled())
	env, err := plansync.EnvelopeFromPlan(gotCanon, plansync.EnvelopeOptions{
		Scope: plansync.Scope{ProjectID: projectID},
	})
	require.NoError(t, err)
	require.NoError(t, hub.Push(ctx, env))
	_, err = hub.Pull(ctx, plansync.PullQuery{Scope: env.Scope})
	require.NoError(t, err)
	_, err = hub.Discover(ctx, env.Scope, []planstore.PlanStatus{planstore.PlanStatusInProgress})
	require.NoError(t, err)
	// Repo export Syncer must remain distinct from Hub PlanSyncProvider.
	var anySync any = &planexport.Syncer{}
	_, ok := anySync.(plansync.PlanSyncProvider)
	require.False(t, ok)

	// Hub seam fields stay unset by local operations.
	require.Nil(t, gotCanon.SyncedAt)
	require.Empty(t, gotCanon.RemoteID)
	require.Nil(t, afterFail.SyncedAt)
	require.Empty(t, afterFail.RemoteID)
}

func writeLegacyYAML(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
}

func ptrTime(t time.Time) *time.Time { return &t }

// acceptFakeGit is a planexport.GitHost for Phase 12 acceptance.
type acceptFakeGit struct {
	identity  string
	prURL     string
	sha       string
	files     map[string][]byte
	prCalls   int
	pushed    bool
	repoDirty bool // operator working tree only; sync worktree stays clean
	authErr   error
	repoPath  string // when set, WorkingTreeDirty is true only for this path
}

func (f *acceptFakeGit) WorkingTreeDirty(_ context.Context, path string) (bool, error) {
	if !f.repoDirty {
		return false, nil
	}
	if f.repoPath != "" {
		return path == f.repoPath, nil
	}
	// Sync worktrees use a fixed token; treat anything else as the operator tree.
	return !strings.Contains(path, "warden-phase12-sync-wt"), nil
}
func (f *acceptFakeGit) RepositoryIdentity(context.Context, string) (string, error) {
	return f.identity, nil
}
func (f *acceptFakeGit) CheckGitHubAuth(context.Context, string) error { return f.authErr }
func (f *acceptFakeGit) EnsureSyncWorktree(context.Context, string, string, string) (string, func(), bool, error) {
	return "/tmp/warden-phase12-sync-wt", func() {}, false, nil
}
func (f *acceptFakeGit) ReadFile(_ context.Context, _, rel string) (bool, []byte, error) {
	if f.files == nil {
		return false, nil, nil
	}
	data, ok := f.files[rel]
	return ok, data, nil
}
func (f *acceptFakeGit) WriteFile(_ context.Context, _, rel string, data []byte) error {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[rel] = append([]byte(nil), data...)
	return nil
}
func (f *acceptFakeGit) CommitPath(context.Context, string, string, string) (string, error) {
	if f.sha == "" {
		f.sha = "phase12sha"
	}
	return f.sha, nil
}
func (f *acceptFakeGit) PushBranch(context.Context, string, string) error {
	f.pushed = true
	return nil
}
func (f *acceptFakeGit) HeadSHA(context.Context, string) (string, error) { return f.sha, nil }
func (f *acceptFakeGit) CreateOrReusePR(context.Context, string, planexport.PRRequest) (planexport.PRInfo, error) {
	f.prCalls++
	url := f.prURL
	if url == "" {
		url = "https://example.test/pull/1"
	}
	return planexport.PRInfo{URL: url, Created: true}, nil
}
