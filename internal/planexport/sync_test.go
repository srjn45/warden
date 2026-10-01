package planexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

type memPlanStore struct {
	mu    sync.Mutex
	plans map[string]*planstore.Plan
}

func (m *memPlanStore) Get(_ context.Context, id string) (*planstore.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[id]
	if !ok {
		return nil, planstore.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memPlanStore) Update(_ context.Context, id string, fn func(*planstore.Plan) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[id]
	if !ok {
		return planstore.ErrNotFound
	}
	return fn(p)
}

type memExportStore struct {
	mu   sync.Mutex
	byID map[string]*Record
}

func (m *memExportStore) Upsert(_ context.Context, rec *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.ID == "" {
		rec.ID = RecordID(rec.PlanID, rec.Repository, rec.TargetRef, rec.OutputPath)
	}
	cp := *rec
	m.byID[rec.ID] = &cp
	return nil
}

func (m *memExportStore) Get(_ context.Context, id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return nil, ErrRecordNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *memExportStore) Find(ctx context.Context, planID, repository, targetRef, outputPath string) (*Record, error) {
	return m.Get(ctx, RecordID(planID, repository, targetRef, outputPath))
}

func (m *memExportStore) ListByPlan(_ context.Context, planID string) ([]*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Record
	for _, r := range m.byID {
		if r.PlanID == planID {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memExportStore) Close() error { return nil }

// fakeGitHost records Git/GitHub activity for unit tests.
type fakeGitHost struct {
	mu sync.Mutex

	dirtyRepo     bool
	dirtyWorktree bool
	identity      string
	authErr       error

	remoteBranches map[string]bool
	files          map[string][]byte // rel path → content in worktree
	headSHA        string
	pushErr        error
	prURL          string
	prCreated      bool
	prErr          error

	pushed          bool
	prCalls         int
	worktreeCreates int
	committedPath   string
	lastBranch      string
	wrotePath       string
}

func newFakeGit() *fakeGitHost {
	return &fakeGitHost{
		identity:       "github.com/example/repo",
		remoteBranches: map[string]bool{},
		files:          map[string][]byte{},
		headSHA:        "abc123deadbeef",
		prURL:          "https://github.com/example/repo/pull/42",
		prCreated:      true,
	}
}

func (f *fakeGitHost) WorkingTreeDirty(_ context.Context, dir string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(dir, "plan-sync") || strings.Contains(dir, ".worktrees") {
		return f.dirtyWorktree, nil
	}
	return f.dirtyRepo, nil
}

func (f *fakeGitHost) RepositoryIdentity(context.Context, string) (string, error) {
	return f.identity, nil
}

func (f *fakeGitHost) CheckGitHubAuth(context.Context, string) error {
	return f.authErr
}

func (f *fakeGitHost) EnsureSyncWorktree(_ context.Context, _, branch, _ string) (string, func(), bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.worktreeCreates++
	f.lastBranch = branch
	remote := f.remoteBranches[branch]
	return filepath.Join(os.TempDir(), "fake-plan-sync-wt"), func() {}, remote, nil
}

func (f *fakeGitHost) ReadFile(_ context.Context, _, relPath string) (bool, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[relPath]
	if !ok {
		return false, nil, nil
	}
	return true, append([]byte(nil), data...), nil
}

func (f *fakeGitHost) WriteFile(_ context.Context, _, relPath string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[relPath] = append([]byte(nil), data...)
	f.wrotePath = relPath
	return nil
}

func (f *fakeGitHost) CommitPath(_ context.Context, _, relPath, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committedPath = relPath
	return f.headSHA, nil
}

func (f *fakeGitHost) PushBranch(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return f.pushErr
	}
	f.pushed = true
	return nil
}

func (f *fakeGitHost) HeadSHA(context.Context, string) (string, error) {
	return f.headSHA, nil
}

func (f *fakeGitHost) CreateOrReusePR(context.Context, string, PRRequest) (PRInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prCalls++
	if f.prErr != nil {
		return PRInfo{}, f.prErr
	}
	return PRInfo{URL: f.prURL, Created: f.prCreated}, nil
}

func testPlan(t *testing.T) *planstore.Plan {
	t.Helper()
	p := &planstore.Plan{
		ID:        "plan-deadbeef",
		ProjectID: "/tmp/proj",
		Name:      "Ship Feature",
		Goal:      "land the feature",
		Status:    planstore.PlanStatusPending,
		Revision:  3,
		Tasks: []planstore.PlanTask{
			{ID: "t1", Prompt: "implement"},
		},
		Constraints: []string{"no force-push to main"},
		DoneWhen:    []string{"PR merged"},
	}
	p.ContentHash = planstore.ComputeContentHash(p)
	return p
}

func newTestSyncer(t *testing.T, plan *planstore.Plan, git *fakeGitHost) (*Syncer, *memExportStore) {
	t.Helper()
	plans := &memPlanStore{plans: map[string]*planstore.Plan{plan.ID: plan}}
	exports := &memExportStore{byID: map[string]*Record{}}
	return &Syncer{
		Plans:    plans,
		Exports:  exports,
		PlansMut: plans,
		Git:      git,
		Now:      func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
	}, exports
}

func TestSync_SuccessIsolatesFromDirtyOperatorTree(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	git.dirtyRepo = true // operator WIP must not block or be staged
	syncer, exports := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID:    plan.ID,
		RepoPath:  "/repo",
		TargetRef: "main",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	require.True(t, git.pushed)
	require.Equal(t, 1, git.prCalls)
	require.Equal(t, "plans/pending/ship-feature.yaml", git.committedPath)
	require.Equal(t, SyncBranch(plan.ID, plan.Revision), res.Branch)
	// Dirty operator tree is ignored (isolated worktree); reason may note
	// deleted-remote recreation when the sync branch is new.
	require.False(t, res.Reused)
	require.True(t, git.dirtyRepo, "fixture sanity: operator tree was dirty")

	rec, err := exports.Find(context.Background(), plan.ID, git.identity, "main", res.OutputPath)
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, rec.Outcome)
	require.Equal(t, res.PRURL, rec.PRURL)
}

func TestSync_IdempotentReuseSkipsGitHub(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	syncer, exports := newTestSyncer(t, plan, git)
	path := ExportPath(plan.Status, plan.Name)
	require.NoError(t, exports.Upsert(context.Background(), &Record{
		PlanID: plan.ID, Repository: git.identity, TargetRef: "main", OutputPath: path,
		Revision: plan.Revision, ContentHash: plan.ContentHash,
		PRURL: "https://github.com/example/repo/pull/7", CommitSHA: "oldsha",
		Outcome: OutcomeSuccess, ExportedAt: time.Now().UTC(),
	}))

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, res.Outcome)
	require.True(t, res.Reused)
	require.Equal(t, ReasonIdempotentReuse, res.Reason)
	require.Equal(t, "https://github.com/example/repo/pull/7", res.PRURL)
	require.Equal(t, 0, git.worktreeCreates)
	require.Equal(t, 0, git.prCalls)
	require.False(t, git.pushed)
}

func TestSync_GitHubAuthUnavailable(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	git.authErr = errors.New("not logged into any GitHub hosts")
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrGitHubAuth)
	require.Equal(t, OutcomeFailed, res.Outcome)
	require.Equal(t, ReasonGitHubAuth, res.Reason)
	require.Equal(t, 0, git.prCalls)
}

func TestSync_ExistingOpenPR(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	git.prCreated = false
	git.prURL = "https://github.com/example/repo/pull/99"
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "develop",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	require.False(t, res.PRCreated)
	require.Equal(t, ReasonExistingOpenPR, res.Reason)
	require.Equal(t, "https://github.com/example/repo/pull/99", res.PRURL)
}

func TestSync_BranchDivergenceNoForcePush(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	git.remoteBranches[SyncBranch(plan.ID, plan.Revision)] = true
	git.pushErr = fmt.Errorf("git push: exit 1: ! [rejected] non-fast-forward")
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSyncConflict)
	require.Equal(t, OutcomeConflict, res.Outcome)
	require.Equal(t, ReasonBranchDivergence, res.Reason)
	require.Equal(t, 0, git.prCalls)
}

func TestSync_DeletedRemoteBranchRecreated(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	// remoteBranches empty ⇒ remoteExisted=false
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	require.Equal(t, ReasonDeletedRemoteOK, res.Reason)
	require.True(t, git.pushed)
}

func TestSync_PathCollisionWithNonWardenFile(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	path := ExportPath(plan.Status, plan.Name)
	git.files[path] = []byte("name: foreign\ngoal: not a warden export\n")
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSyncConflict)
	require.Equal(t, OutcomeConflict, res.Outcome)
	require.Equal(t, ReasonPathCollision, res.Reason)
	require.False(t, git.pushed)
	require.Equal(t, 0, git.prCalls)
}

func TestSync_OverwritesPriorWardenExport(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	path := ExportPath(plan.Status, plan.Name)
	git.files[path] = []byte("# warden-plan-export: replica only — not authoritative\nplan_id: plan-deadbeef\nrevision: 2\n")
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main",
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	require.Equal(t, path, git.wrotePath)
	require.True(t, git.pushed)
}

func TestSync_JSONFormatWritesJSONPath(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main", Format: FormatJSON,
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	wantPath := ExportPathFormat(plan.Status, plan.Name, FormatJSON)
	require.Equal(t, wantPath, res.OutputPath)
	require.Equal(t, wantPath, git.committedPath)
	require.Contains(t, string(git.files[wantPath]), `"warden_plan_export"`)
	require.Contains(t, string(git.files[wantPath]), `"plan_id": "`+plan.ID+`"`)
}

func TestSync_JSONPathCollisionWithNonWardenFile(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	path := ExportPathFormat(plan.Status, plan.Name, FormatJSON)
	git.files[path] = []byte(`{"name":"foreign","goal":"not a warden export"}` + "\n")
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main", Format: FormatJSON,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSyncConflict)
	require.Equal(t, OutcomeConflict, res.Outcome)
	require.Equal(t, ReasonPathCollision, res.Reason)
	require.False(t, git.pushed)
}

func TestSync_OverwritesPriorJSONWardenExport(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	path := ExportPathFormat(plan.Status, plan.Name, FormatJSON)
	git.files[path] = []byte("{\n  \"warden_plan_export\": \"replica only — not authoritative\",\n  \"plan_id\": \"plan-deadbeef\",\n  \"revision\": 2\n}\n")
	syncer, _ := newTestSyncer(t, plan, git)

	res, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main", Format: FormatJSON,
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeSuccess, res.Outcome)
	require.Equal(t, path, git.wrotePath)
	require.True(t, git.pushed)
}

func TestSync_UnsupportedFormat(t *testing.T) {
	plan := testPlan(t)
	git := newFakeGit()
	syncer, _ := newTestSyncer(t, plan, git)

	_, err := syncer.Sync(context.Background(), SyncOptions{
		PlanID: plan.ID, RepoPath: "/repo", TargetRef: "main", Format: Format("toml"),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported format")
	require.Equal(t, 0, git.worktreeCreates)
}

func TestNew_FormatFactory(t *testing.T) {
	r, err := New("")
	require.NoError(t, err)
	require.Equal(t, FormatYAML, r.Format())

	r, err = New(FormatYAML)
	require.NoError(t, err)
	require.Equal(t, FormatYAML, r.Format())

	r, err = New(FormatJSON)
	require.NoError(t, err)
	require.Equal(t, FormatJSON, r.Format())

	_, err = New(Format("xml"))
	require.Error(t, err)
}

func TestIsWardenExportForPlan_JSONAndYAML(t *testing.T) {
	require.True(t, isWardenExportForPlan(
		[]byte("# warden-plan-export: replica only — not authoritative\nplan_id: plan-deadbeef\n"),
		"plan-deadbeef",
	))
	require.True(t, isWardenExportForPlan(
		[]byte("{\n  \"warden_plan_export\": \"replica only — not authoritative\",\n  \"plan_id\": \"plan-deadbeef\"\n}\n"),
		"plan-deadbeef",
	))
	require.False(t, isWardenExportForPlan(
		[]byte("{\n  \"warden_plan_export\": \"replica only — not authoritative\",\n  \"plan_id\": \"plan-other\"\n}\n"),
		"plan-deadbeef",
	))
	require.False(t, isWardenExportForPlan([]byte(`{"name":"foreign"}`), "plan-deadbeef"))
}

func TestSyncBranchNaming(t *testing.T) {
	require.Equal(t, "warden/plan-sync/plan-deadbeef/3", SyncBranch("plan-deadbeef", 3))
}
