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
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) //nolint:errcheck
	http.Post(ts.URL+"/pipelines/demo/start", "application/json", nil)              //nolint:errcheck

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

func TestPipelineEditJobRoute(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) // job "a", pending

	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/edit", "application/json", strings.NewReader(`{"prompt":"new prompt"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Prompt != "new prompt" {
		t.Fatalf("prompt not edited: %q", got.Job("a").Prompt)
	}
}

func TestPipelineEditJobNothing400(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) //nolint:errcheck
	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/edit", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty edit want 400, got %d", resp.StatusCode)
	}
}

func TestPipelineRetryRoute(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	// force job a into a failed state.
	ps.Update("demo", func(p *pipeline.Pipeline) {
		p.Job("a").Status = pipeline.JobFailed
		p.Status = pipeline.StatusStalled
	})

	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("retried job should be running, got %s", got.Job("a").Status)
	}
}

func TestPipelineRetryNotRetryable409(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) //nolint:errcheck // a pending
	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retry pending want 409, got %d", resp.StatusCode)
	}
}

func TestPipelineCancelSkipsNeedsAttention(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	ps.Update("demo", func(p *pipeline.Pipeline) {
		p.Job("a").Status = pipeline.JobNeedsAttention
		p.Job("a").SessionID = "demo-a"
		p.Status = pipeline.StatusRunning
	})
	resp, err := http.Post(ts.URL+"/pipelines/demo/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobSkipped {
		t.Fatalf("needs_attention job should be skipped on cancel, got %s", got.Job("a").Status)
	}
	if got.Status != pipeline.StatusCanceled {
		t.Fatalf("pipeline should be canceled, got %s", got.Status)
	}
}

func TestPipelineDeleteRoute(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) // job "a" pending

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pipelines/demo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status %d", resp.StatusCode)
	}
	if _, gerr := ps.Get("demo"); gerr == nil {
		t.Fatalf("pipeline should be gone after delete")
	}
}

func TestPipelineDeleteRefusesLiveJob(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	ps.Update("demo", func(p *pipeline.Pipeline) { p.Job("a").Status = pipeline.JobRunning })

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/pipelines/demo", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete with a live job want 409, got %d", resp.StatusCode)
	}
}
