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
	"github.com/srjn45/warden/internal/plansync"
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

func TestHubPlanSyncRoutes_PushSuccessAndConflict(t *testing.T) {
	ts := newHubPlanSyncRouteServer(t)
	hub, err := plansync.NewHub(plansync.HubOptions{BaseURL: ts.URL, Token: "hub-secret", HTTP: ts.Client()})
	require.NoError(t, err)

	env := hubRouteEnvelope("push", planstore.PlanStatusPending, "org", "team", "project")
	ack, err := hub.PushEnvelope(context.Background(), env)
	require.NoError(t, err)
	require.NotEmpty(t, ack.RemoteID)
	require.NotNil(t, ack.SyncedAt)

	conflicting := env
	conflicting.Revision = 2
	conflicting.ContentHash = "sha256:push-conflict"
	conflicting.ConflictToken = plansync.ConflictToken(conflicting.Revision, conflicting.ContentHash)
	_, err = hub.PushEnvelope(context.Background(), conflicting)
	var conflict *plansync.ConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, env.PlanID, conflict.PlanID)
	require.Equal(t, env.ConflictToken, conflict.Expected)
	require.Equal(t, conflicting.ConflictToken, conflict.Actual)
}

func TestHubPlanSyncRoutes_PullFilters(t *testing.T) {
	ts := newHubPlanSyncRouteServer(t)
	hub, err := plansync.NewHub(plansync.HubOptions{BaseURL: ts.URL, Token: "hub-secret", HTTP: ts.Client()})
	require.NoError(t, err)

	matching := hubRouteEnvelope("matching", planstore.PlanStatusInProgress, "org", "team", "project")
	for _, env := range []plansync.Envelope{
		matching,
		hubRouteEnvelope("wrong-status", planstore.PlanStatusCompleted, "org", "team", "project"),
		hubRouteEnvelope("wrong-team", planstore.PlanStatusInProgress, "org", "other-team", "project"),
	} {
		require.NoError(t, hub.Push(context.Background(), env))
	}

	got, err := hub.Pull(context.Background(), plansync.PullQuery{
		Scope: matching.Scope, Statuses: []planstore.PlanStatus{planstore.PlanStatusInProgress},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, matching.PlanID, got[0].PlanID)

	got, err = hub.Pull(context.Background(), plansync.PullQuery{Scope: matching.Scope, PlanID: "wrong-status"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "wrong-status", got[0].PlanID)
}

func TestHubPlanSyncRoutes_DiscoverFiltersAndAuthRejection(t *testing.T) {
	ts := newHubPlanSyncRouteServer(t)
	hub, err := plansync.NewHub(plansync.HubOptions{BaseURL: ts.URL, Token: "hub-secret", HTTP: ts.Client()})
	require.NoError(t, err)

	scope := plansync.Scope{OrganizationID: "org", TeamID: "team", ProjectID: "project"}
	for _, env := range []plansync.Envelope{
		hubRouteEnvelope("pending", planstore.PlanStatusPending, "org", "team", "project"),
		hubRouteEnvelope("running", planstore.PlanStatusInProgress, "org", "team", "project"),
		hubRouteEnvelope("completed", planstore.PlanStatusCompleted, "org", "team", "project"),
		hubRouteEnvelope("other-project", planstore.PlanStatusPending, "org", "team", "other-project"),
	} {
		require.NoError(t, hub.Push(context.Background(), env))
	}

	got, err := hub.Discover(context.Background(), scope, nil)
	require.NoError(t, err)
	require.Len(t, got, 2, "empty discovery filter defaults to pending and in_progress")
	require.ElementsMatch(t, []string{"pending", "running"}, []string{got[0].PlanID, got[1].PlanID})

	got, err = hub.Discover(context.Background(), scope, []planstore.PlanStatus{planstore.PlanStatusCompleted})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "completed", got[0].PlanID)

	resp, err := http.Post(ts.URL+plansync.PathDiscover, "application/json", bytes.NewReader(mustPlanSyncJSON(t, map[string]any{"scope": scope})))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	readonly, err := plansync.NewHub(plansync.HubOptions{BaseURL: ts.URL, Token: "readonly", HTTP: ts.Client()})
	require.NoError(t, err)
	_, err = readonly.Discover(context.Background(), scope, nil)
	require.Error(t, err, "read-only bearer token cannot discover")
}

func newHubPlanSyncRouteServer(t *testing.T) *httptest.Server {
	t.Helper()
	hubStore, err := plansync.NewFileHubStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, hubStore.Close()) })
	srv := &Server{store: newFakeStore(), life: &fakeLife{}}
	srv.SetHubPlanSyncStore(hubStore)
	srv.SetAuth("hub-secret", "readonly")
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts
}

func hubRouteEnvelope(id string, status planstore.PlanStatus, org, team, project string) plansync.Envelope {
	hash := "sha256:" + id
	return plansync.Envelope{
		SchemaVersion: plansync.SchemaVersion,
		Scope:         plansync.Scope{OrganizationID: org, TeamID: team, ProjectID: project},
		ProjectID:     project, PlanID: id, Revision: 1, ContentHash: hash,
		Visibility: plansync.VisibilityTeam, Lifecycle: status,
		ConflictToken: plansync.ConflictToken(1, hash),
	}
}

func mustPlanSyncJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
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
