package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/stretchr/testify/require"
)

func newPipeServer(t *testing.T) (*httptest.Server, *pipeline.Store) {
	t.Helper()
	ps, _ := pipeline.NewStore(t.TempDir())
	cs, _ := ctxstore.New(t.TempDir())
	ss := newFakeStore()
	exec := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	srv := &Server{store: ss, life: &fakeLife{}, exec: exec, hub: newHub(), done: make(chan struct{})}
	return httptest.NewServer(srv.router()), ps
}

const yamlBody = `{"spec":"name: demo\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`

func TestPipelineCreateThenList(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	var p pipeline.Pipeline
	json.NewDecoder(resp.Body).Decode(&p)
	if p.ID != "demo" || len(p.Jobs) != 1 {
		t.Fatalf("created pipeline wrong: %+v", p)
	}

	resp2, err := http.Get(ts.URL + "/pipelines")
	require.NoError(t, err)
	defer resp2.Body.Close()
	var lr struct {
		Pipelines []pipeline.Pipeline `json:"pipelines"`
	}
	json.NewDecoder(resp2.Body).Decode(&lr)
	if len(lr.Pipelines) != 1 {
		t.Fatalf("list want 1, got %d", len(lr.Pipelines))
	}
}

func TestPipelineCreateInvalidYAML400(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	bad := `{"spec":"name: demo\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    depends_on: [ghost]\n"}`
	resp, err := http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(bad))
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestPipelineStartSpawnsRoot(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) //nolint:errcheck

	resp, err := http.Post(ts.URL+"/pipelines/demo/start", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("root not spawned on start: %+v", got.Job("a"))
	}
}

func TestPipelineEmitMarksDone(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))   //nolint:errcheck
	http.Post(ts.URL+"/pipelines/demo/start", "application/json", nil)                 //nolint:errcheck

	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/emit", "application/json", strings.NewReader(`{"text":"all done"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("emit status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobDone || got.Job("a").Output != "all done" {
		t.Fatalf("emit did not complete job: %+v", got.Job("a"))
	}
	if got.Status != pipeline.StatusDone {
		t.Fatalf("single-job pipeline should be done, got %s", got.Status)
	}
}

func TestPipelineShow404(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/pipelines/ghost")
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	_ = context.Background()
}
