package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
)

func TestSyncPlanToRepoRoute_IdempotentWithFakeGit(t *testing.T) {
	root := t.TempDir()
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	exports, err := planexport.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = exports.Close() })

	plan := &planstore.Plan{
		ID:        planstore.PlanID(root, "sync-route"),
		ProjectID: root,
		Name:      "Sync Route",
		Goal:      "exercise API",
		Status:    planstore.PlanStatusPending,
		Revision:  2,
		Tasks:     []planstore.PlanTask{{ID: "t1", Prompt: "sync"}},
	}
	plan.ContentHash = planstore.ComputeContentHash(plan)
	require.NoError(t, ps.Create(context.Background(), plan))

	git := &routeFakeGit{
		identity: "github.com/example/warden",
		prURL:    "https://github.com/example/warden/pull/11",
		sha:      "deadbeefcafebabe",
	}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps, planExports: exports, planSyncGit: git}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	body := []byte(`{"target_ref":"main"}`)
	resp, err := http.Post(ts.URL+"/api/v1/plans/"+plan.ID+"/sync_to_repo", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var first map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&first))
	require.Equal(t, "success", first["outcome"])
	require.Equal(t, false, first["reused"])
	require.Equal(t, "https://github.com/example/warden/pull/11", first["pr_url"])
	require.Equal(t, float64(2), first["revision"])
	require.Equal(t, 1, git.prCalls)

	resp2, err := http.Post(ts.URL+"/api/v1/plans/"+plan.ID+"/sync_to_repo", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	var second map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&second))
	require.Equal(t, "skipped", second["outcome"])
	require.Equal(t, true, second["reused"])
	require.Equal(t, 1, git.prCalls, "idempotent reuse must not open another PR")
}

func TestSyncPlanToRepoRoute_PathCollision409(t *testing.T) {
	root := t.TempDir()
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	exports, err := planexport.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = exports.Close() })

	plan := &planstore.Plan{
		ID: planstore.PlanID(root, "collision"), ProjectID: root, Name: "Collision",
		Goal: "block overwrite", Status: planstore.PlanStatusPending, Revision: 1,
		Tasks: []planstore.PlanTask{{ID: "t1", Prompt: "x"}},
	}
	plan.ContentHash = planstore.ComputeContentHash(plan)
	require.NoError(t, ps.Create(context.Background(), plan))

	git := &routeFakeGit{
		identity: "github.com/example/warden",
		files: map[string][]byte{
			"plans/pending/collision.yaml": []byte("name: foreign\n"),
		},
	}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps, planExports: exports, planSyncGit: git}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/plans/"+plan.ID+"/sync_to_repo", "application/json",
		bytes.NewReader([]byte(`{"target_ref":"main"}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "conflict", body["outcome"])
	require.Equal(t, "path_collision", body["reason"])
}

func TestSyncPlanToRepoRoute_JSONFormat(t *testing.T) {
	root := t.TempDir()
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	exports, err := planexport.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = exports.Close() })

	plan := &planstore.Plan{
		ID:        planstore.PlanID(root, "json-sync"),
		ProjectID: root,
		Name:      "JSON Sync",
		Goal:      "json replica",
		Status:    planstore.PlanStatusPending,
		Revision:  1,
		Tasks:     []planstore.PlanTask{{ID: "t1", Prompt: "sync"}},
	}
	plan.ContentHash = planstore.ComputeContentHash(plan)
	require.NoError(t, ps.Create(context.Background(), plan))

	git := &routeFakeGit{
		identity: "github.com/example/warden",
		prURL:    "https://github.com/example/warden/pull/22",
		sha:      "jsoncafebabe",
		files:    map[string][]byte{},
	}
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps, planExports: exports, planSyncGit: git}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/plans/"+plan.ID+"/sync_to_repo", "application/json",
		bytes.NewReader([]byte(`{"target_ref":"main","format":"json"}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "success", body["outcome"])
	wantPath := planexport.ExportPathFormat(plan.Status, plan.Name, planexport.FormatJSON)
	require.Equal(t, wantPath, body["output_path"])
	raw, ok := git.files[wantPath]
	require.True(t, ok)
	require.Contains(t, string(raw), `"warden_plan_export"`)
	require.Contains(t, string(raw), `"plan_id": "`+plan.ID+`"`)
}

type routeFakeGit struct {
	identity string
	prURL    string
	sha      string
	files    map[string][]byte
	prCalls  int
	pushed   bool
}

func (f *routeFakeGit) WorkingTreeDirty(context.Context, string) (bool, error) { return false, nil }
func (f *routeFakeGit) RepositoryIdentity(context.Context, string) (string, error) {
	return f.identity, nil
}
func (f *routeFakeGit) CheckGitHubAuth(context.Context, string) error { return nil }
func (f *routeFakeGit) EnsureSyncWorktree(context.Context, string, string, string) (string, func(), bool, error) {
	return tTempWorktree(), func() {}, false, nil
}
func (f *routeFakeGit) ReadFile(_ context.Context, _, rel string) (bool, []byte, error) {
	if f.files == nil {
		return false, nil, nil
	}
	data, ok := f.files[rel]
	return ok, data, nil
}
func (f *routeFakeGit) WriteFile(_ context.Context, _, rel string, data []byte) error {
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[rel] = append([]byte(nil), data...)
	return nil
}
func (f *routeFakeGit) CommitPath(context.Context, string, string, string) (string, error) {
	if f.sha == "" {
		f.sha = "abc123"
	}
	return f.sha, nil
}
func (f *routeFakeGit) PushBranch(context.Context, string, string) error {
	f.pushed = true
	return nil
}
func (f *routeFakeGit) HeadSHA(context.Context, string) (string, error) { return f.sha, nil }
func (f *routeFakeGit) CreateOrReusePR(context.Context, string, planexport.PRRequest) (planexport.PRInfo, error) {
	f.prCalls++
	url := f.prURL
	if url == "" {
		url = "https://example.test/pull/1"
	}
	return planexport.PRInfo{URL: url, Created: true}, nil
}

func tTempWorktree() string {
	// Stable-enough path token for dirty-tree classification in Syncer.
	return "/tmp/warden-plan-sync-test-wt-" + time.Now().Format("150405")
}
