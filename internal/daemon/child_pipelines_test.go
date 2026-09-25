package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// childPipelines fetches an agent's ChildPipelines[] forward-edge list from the store.
func childPipelines(t *testing.T, st store.Store, id string) []string {
	t.Helper()
	a, err := st.Get(context.Background(), id)
	require.NoError(t, err)
	return a.ChildPipelines
}

// TestPipelineParentEdgeInvariant is the bidirectional-edge invariant test for
// pipeline ownership (spec §6.1/D4/D6): a pipeline's owning-agent relationship is
// stored on BOTH ends. It drives the edge through create (add) and delete (remove)
// and asserts the two ends agree: Pipeline.ParentAgentID <-> agent.ChildPipelines[].
func TestPipelineParentEdgeInvariant(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	owner := &store.Session{ID: "agent-owner", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, owner))

	// --- create: add edge (both ends) ---
	p1 := &pipeline.Pipeline{ID: "pipe-1", ParentAgentID: "agent-owner"}
	s.addPipelineParentEdge(ctx, p1)
	require.Equal(t, []string{"pipe-1"}, childPipelines(t, st, "agent-owner"))

	// idempotent: a re-add (e.g. recovery re-create) does not duplicate.
	s.addPipelineParentEdge(ctx, p1)
	require.Equal(t, []string{"pipe-1"}, childPipelines(t, st, "agent-owner"))

	// a second owned pipeline accumulates.
	p2 := &pipeline.Pipeline{ID: "pipe-2", ParentAgentID: "agent-owner"}
	s.addPipelineParentEdge(ctx, p2)
	require.ElementsMatch(t, []string{"pipe-1", "pipe-2"}, childPipelines(t, st, "agent-owner"))

	// --- delete: remove edge (both ends) ---
	s.removePipelineParentEdge(ctx, p1)
	require.Equal(t, []string{"pipe-2"}, childPipelines(t, st, "agent-owner"))
	// removing the last owned pipeline clears the list entirely (omitempty).
	s.removePipelineParentEdge(ctx, p2)
	require.Nil(t, childPipelines(t, st, "agent-owner"))
}

// TestPipelineParentEdgeExclusions checks the no-op gating: an operator-created
// pipeline (empty ParentAgentID) never populates any ChildPipelines[] list, and
// the helpers never panic on nil.
func TestPipelineParentEdgeExclusions(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	owner := &store.Session{ID: "agent-owner", Status: store.StatusWorking}
	require.NoError(t, st.Insert(ctx, owner))

	require.NotPanics(t, func() {
		s.addPipelineParentEdge(ctx, &pipeline.Pipeline{ID: "pipe-op"}) // operator-created (no owner)
		s.addPipelineParentEdge(ctx, nil)
		s.removePipelineParentEdge(ctx, &pipeline.Pipeline{ID: "pipe-op"})
		s.removePipelineParentEdge(ctx, nil)
	})
	require.Nil(t, childPipelines(t, st, "agent-owner"), "an operator-created pipeline must not populate any forward edge")
}

// TestPipelineParentEdgeDanglingOwner tolerates a missing owning-agent record
// (§6.3): the add/remove are logged no-ops, never fatal.
func TestPipelineParentEdgeDanglingOwner(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFileStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close(ctx) })
	s := &Server{store: st}

	p := &pipeline.Pipeline{ID: "pipe-ghost", ParentAgentID: "ghost-agent"}
	require.NotPanics(t, func() {
		s.addPipelineParentEdge(ctx, p)
		s.removePipelineParentEdge(ctx, p)
	})
}

// newPipeServerWithStore builds a server wired with the pipeline executor and a
// fake session store, returning the store so tests can inspect the owning agent's
// ChildPipelines[] forward edge after create/delete.
func newPipeServerWithStore(t *testing.T) (*httptest.Server, *fakeStore) {
	t.Helper()
	ps, _ := pipeline.NewStore(t.TempDir())
	cs, _ := ctxstore.New(t.TempDir())
	ss := newFakeStore()
	exec := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	srv := &Server{store: ss, life: &fakeLife{}, exec: exec, hub: newHub(), done: make(chan struct{})}
	return httptest.NewServer(srv.router()), ss
}

// postPipeline POSTs a create request with an optional actor header and returns
// the decoded pipeline.
func postPipeline(t *testing.T, url, actor, body string) pipeline.Pipeline {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/v1/pipelines", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set(auth.ActorHeader, actor)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equalf(t, http.StatusCreated, resp.StatusCode, "create pipeline")
	var p pipeline.Pipeline
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	return p
}

// TestPipelineCreateStampsParentAgent exercises the create/delete route path (D4/D6):
// parent_agent_id is resolved (request-body override > actor identity), stamped on
// the pipeline, and mirrored on the owning agent's ChildPipelines[] — with delete
// dropping it. Operator and terminal callers own no pipeline.
func TestPipelineCreateStampsParentAgent(t *testing.T) {
	ts, ss := newPipeServerWithStore(t)
	defer ts.Close()
	ctx := context.Background()

	owner := &store.Session{ID: "agent-owner", Status: store.StatusWorking}
	require.NoError(t, ss.Insert(ctx, owner))
	other := &store.Session{ID: "agent-other", Status: store.StatusWorking}
	require.NoError(t, ss.Insert(ctx, other))
	term := &store.Session{ID: "term-1", Kind: store.KindTerminal, Status: store.StatusWorking}
	require.NoError(t, ss.Insert(ctx, term))

	spec := func(name string) string {
		return "name: " + name + "\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"
	}

	// 1. Actor identity (the agent behind the request) becomes the owner.
	p1 := postPipeline(t, ts.URL, "agent-owner", `{"spec":"`+spec("p1")+`"}`)
	require.Equal(t, "agent-owner", p1.ParentAgentID)
	require.Contains(t, childPipelines(t, ss, "agent-owner"), "p1")

	// 2. Explicit request-body parent_agent_id wins over the actor identity.
	p2 := postPipeline(t, ts.URL, "agent-owner", `{"parent_agent_id":"agent-other","spec":"`+spec("p2")+`"}`)
	require.Equal(t, "agent-other", p2.ParentAgentID)
	require.Contains(t, childPipelines(t, ss, "agent-other"), "p2")
	require.NotContains(t, childPipelines(t, ss, "agent-owner"), "p2", "body override must not credit the actor")

	// 3. Operator create (no actor header) → no owning agent, no edge.
	pOp := postPipeline(t, ts.URL, "", `{"spec":"`+spec("p3")+`"}`)
	require.Equal(t, "", pOp.ParentAgentID)

	// 4. A terminal caller never owns a pipeline (§6.4).
	pTerm := postPipeline(t, ts.URL, "term-1", `{"spec":"`+spec("p4")+`"}`)
	require.Equal(t, "", pTerm.ParentAgentID)
	require.Nil(t, childPipelines(t, ss, "term-1"))

	// 5. Deleting an owned pipeline removes it from the owner's ChildPipelines[].
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/pipelines/p1", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	require.NoError(t, err)
	delResp.Body.Close()
	require.Equal(t, http.StatusOK, delResp.StatusCode)
	require.NotContains(t, childPipelines(t, ss, "agent-owner"), "p1")
}
