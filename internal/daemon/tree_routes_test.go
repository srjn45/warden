package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/tree"
	"github.com/stretchr/testify/require"
)

// getTree drives the strict handler and returns the 200 body, failing the test
// if the response was not a 200.
func getTree(t *testing.T, srv *Server, params oapi.GetTreeParams) oapi.GetTree200JSONResponse {
	t.Helper()
	resp, err := srv.GetTree(context.Background(), oapi.GetTreeRequestObject{Params: params})
	require.NoError(t, err)
	ok, is := resp.(oapi.GetTree200JSONResponse)
	require.True(t, is, "expected 200, got %T", resp)
	return ok
}

// findRoot returns the root node with the given id, or nil.
func findRoot(tr tree.Tree, id string) *tree.Node {
	for _, r := range tr.Roots {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// TestGetTreeSyntheticBucket: a bare session with no project/location lands in
// the synthetic "No project" bucket as an agent node, and the response carries
// Cache-Control: no-store.
func TestGetTreeSyntheticBucket(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Name: "orch", Status: store.StatusWorking}
	srv := &Server{store: fs}

	resp := getTree(t, srv, oapi.GetTreeParams{})
	require.Equal(t, "no-store", resp.Headers.CacheControl)

	bucket := findRoot(resp.Body, "project:__none__")
	require.NotNil(t, bucket, "expected synthetic bucket root")
	require.True(t, bucket.Detail.Synthetic)
	require.Len(t, bucket.Children, 1)
	require.Equal(t, tree.NodeTypeAgent, bucket.Children[0].Type)
	require.Equal(t, "A-1", bucket.Children[0].SessionID)
}

// TestGetTreeEmptyFleet: an empty fleet returns roots as [] (never null).
func TestGetTreeEmptyFleet(t *testing.T) {
	srv := &Server{store: newFakeStore()}
	resp := getTree(t, srv, oapi.GetTreeParams{})
	require.NotNil(t, resp.Body.Roots)
	require.Len(t, resp.Body.Roots, 0)
}

// TestGetTreeUnknownProjectEmpty: an unknown ?project_id= returns 200 with empty
// roots — not 404 (locked decision; spec §9 Q-T2).
func TestGetTreeUnknownProjectEmpty(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	srv := &Server{store: fs}

	resp := getTree(t, srv, oapi.GetTreeParams{ProjectId: "/nope/does-not-exist"})
	require.NotNil(t, resp.Body.Roots)
	require.Len(t, resp.Body.Roots, 0)
}

// TestGetTreeProjectScope: ?project_id= scoped to a loose-dir group returns only
// that subtree (and never the synthetic bucket).
func TestGetTreeProjectScope(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking, Repo: "/work/repo"}
	fs.data["B-2"] = &store.Session{ID: "B-2", Status: store.StatusIdle} // synthetic bucket
	srv := &Server{store: fs}

	resp := getTree(t, srv, oapi.GetTreeParams{ProjectId: "/work/repo"})
	require.Len(t, resp.Body.Roots, 1)
	require.Equal(t, "project:/work/repo", resp.Body.Roots[0].ID)
	require.Nil(t, findRoot(resp.Body, "project:__none__"))
}

// TestGetTreeAllFilter: system:true sessions are hidden unless ?all=true, exactly
// mirroring GET /sessions.
func TestGetTreeAllFilter(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	fs.data["sys"] = &store.Session{ID: "sys", Status: store.StatusIdle, Tags: []string{"system:true"}}
	srv := &Server{store: fs}

	def := getTree(t, srv, oapi.GetTreeParams{})
	bucket := findRoot(def.Body, "project:__none__")
	require.NotNil(t, bucket)
	require.Len(t, bucket.Children, 1, "system session must be hidden by default")

	all := getTree(t, srv, oapi.GetTreeParams{All: true})
	bucketAll := findRoot(all.Body, "project:__none__")
	require.NotNil(t, bucketAll)
	require.Len(t, bucketAll.Children, 2, "?all=true must include the system session")
}

// TestGetTreeDegradedIs503: a degraded active session scan returns 503, never a
// tree built from a partial fleet (same complete-or-error contract as /sessions).
func TestGetTreeDegradedIs503(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	fs.listErr = degradedErr()
	srv := &Server{store: fs}

	resp, err := srv.GetTree(context.Background(), oapi.GetTreeRequestObject{})
	require.NoError(t, err)
	_, is503 := resp.(oapi.GetTree503JSONResponse)
	require.True(t, is503, "expected 503, got %T", resp)
}

// TestCapabilitiesIncludesProjectTree: the project-tree flag is advertised so
// clients feature-detect the tree endpoint + SSE event.
func TestCapabilitiesIncludesProjectTree(t *testing.T) {
	srv := &Server{}
	resp, err := srv.GetCapabilities(context.Background(), oapi.GetCapabilitiesRequestObject{})
	require.NoError(t, err)
	caps, ok := resp.(oapi.GetCapabilities200JSONResponse)
	require.True(t, ok)
	require.Contains(t, caps.Capabilities, "project-tree")
}
