package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/projectstore"
)

func decodeGroup(t *testing.T, resp *http.Response) projectstore.ProjectGroup {
	t.Helper()
	defer resp.Body.Close()
	var g projectstore.ProjectGroup
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&g))
	return g
}

func putJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func deleteReq(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestHandleListProjectGroupsEmpty(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp, err := http.Get(srv.URL + "/api/v1/project-groups")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Groups []projectstore.ProjectGroup `json:"groups"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotNil(t, out.Groups) // never null — encodes as [] when empty
	require.Empty(t, out.Groups)
}

func TestHandleListProjectGroupsUnconfigured(t *testing.T) {
	srv := lifeServer(t, newFakeStore(), &fakeLife{})
	resp, err := http.Get(srv.URL + "/api/v1/project-groups")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestHandleCreateAndGetProjectGroup(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())

	resp := postJSON(t, srv.URL+"/api/v1/project-groups", map[string]any{
		"name": "Backend", "project_ids": []string{"/repos/a", "/repos/b"},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	g := decodeGroup(t, resp)
	require.NotEmpty(t, g.ID) // minted
	require.Equal(t, "Backend", g.Name)
	require.Equal(t, []string{"/repos/a", "/repos/b"}, g.ProjectIDs)

	// GET it back by id.
	gresp, err := http.Get(srv.URL + "/api/v1/project-groups/" + g.ID)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, gresp.StatusCode)
	got := decodeGroup(t, gresp)
	require.Equal(t, g.ID, got.ID)
	require.Equal(t, "Backend", got.Name)

	// It appears in the list.
	lresp, err := http.Get(srv.URL + "/api/v1/project-groups")
	require.NoError(t, err)
	defer lresp.Body.Close()
	var out struct {
		Groups []projectstore.ProjectGroup `json:"groups"`
	}
	require.NoError(t, json.NewDecoder(lresp.Body).Decode(&out))
	require.Len(t, out.Groups, 1)
}

func TestHandleCreateProjectGroupMissingName(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp := postJSON(t, srv.URL+"/api/v1/project-groups", map[string]any{"project_ids": []string{"x"}})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleGetProjectGroupNotFound(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp, err := http.Get(srv.URL + "/api/v1/project-groups/nope")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleUpdateProjectGroup(t *testing.T) {
	srv, ps := projectServer(t, newFakeStore())
	created, err := ps.CreateGroup(projectstore.ProjectGroup{Name: "Old", ProjectIDs: []string{"a"}})
	require.NoError(t, err)

	resp := putJSON(t, srv.URL+"/api/v1/project-groups/"+created.ID, map[string]any{
		"name": "New", "project_ids": []string{"a", "c"},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	g := decodeGroup(t, resp)
	require.Equal(t, "New", g.Name)
	require.Equal(t, []string{"a", "c"}, g.ProjectIDs)
}

func TestHandleUpdateProjectGroupNotFound(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp := putJSON(t, srv.URL+"/api/v1/project-groups/nope", map[string]any{"name": "X"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandleUpdateProjectGroupBlankName(t *testing.T) {
	srv, ps := projectServer(t, newFakeStore())
	created, err := ps.CreateGroup(projectstore.ProjectGroup{Name: "G"})
	require.NoError(t, err)
	resp := putJSON(t, srv.URL+"/api/v1/project-groups/"+created.ID, map[string]any{"name": ""})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleDeleteProjectGroup(t *testing.T) {
	srv, ps := projectServer(t, newFakeStore())
	created, err := ps.CreateGroup(projectstore.ProjectGroup{Name: "G"})
	require.NoError(t, err)

	resp := deleteReq(t, srv.URL+"/api/v1/project-groups/"+created.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Gone now.
	_, err = ps.GetGroup(created.ID)
	require.ErrorIs(t, err, projectstore.ErrGroupNotFound)

	// Idempotent: deleting an unknown id still 200s.
	resp2 := deleteReq(t, srv.URL+"/api/v1/project-groups/nope")
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	resp2.Body.Close()
}
