package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/stretchr/testify/require"
)

func TestListProjects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects", r.URL.Path)
		w.Write([]byte(`{"projects":[{"id":"/repos/alpha","name":"Alpha","path":"/repos/alpha","status":"open"}]}`))
	}))
	defer srv.Close()
	ps, err := New(srv.URL).ListProjects(context.Background())
	require.NoError(t, err)
	require.Len(t, ps, 1)
	require.Equal(t, "Alpha", ps[0].Name)
	require.Equal(t, projectstore.StatusOpen, ps[0].Status)
}

// TestCloseProjectEncodesPathID verifies the id (a filesystem path with slashes) is
// percent-encoded into a single path segment so the daemon's {id} route matches.
func TestCloseProjectEncodesPathID(t *testing.T) {
	var gotRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath() // the encoded form the daemon router sees
		w.Write([]byte(`{"id":"/home/u/repo","name":"Repo","path":"/home/u/repo","status":"closed"}`))
	}))
	defer srv.Close()
	p, err := New(srv.URL).CloseProject(context.Background(), "/home/u/repo")
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusClosed, p.Status)
	require.Equal(t, "/api/v1/projects/%2Fhome%2Fu%2Frepo/close", gotRawPath,
		"the project id is percent-encoded into one path segment")
}

func TestOpenLocalProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/projects/local", r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "/repos/alpha", body["path"])
		require.Equal(t, "Alpha", body["name"])
		w.Write([]byte(`{"id":"/repos/alpha","name":"Alpha","path":"/repos/alpha","status":"open"}`))
	}))
	defer srv.Close()
	p, err := New(srv.URL).OpenLocalProject(context.Background(), "/repos/alpha", "Alpha")
	require.NoError(t, err)
	require.Equal(t, "/repos/alpha", p.ID)
	require.Equal(t, "Alpha", p.Name)
	require.Equal(t, projectstore.StatusOpen, p.Status)
}

func TestOpenRemoteProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/projects/remote", r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "https://github.com/acme/widgets", body["url"])
		require.Equal(t, "", body["name"])
		w.Write([]byte(`{"id":"/work/widgets","name":"widgets","path":"/work/widgets","status":"open"}`))
	}))
	defer srv.Close()
	p, err := New(srv.URL).OpenRemoteProject(context.Background(), "https://github.com/acme/widgets", "")
	require.NoError(t, err)
	require.Equal(t, "/work/widgets", p.ID)
	require.Equal(t, "widgets", p.Name)
	require.Equal(t, projectstore.StatusOpen, p.Status)
}

func TestCreateProject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/projects/new", r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "my-project", body["name"])
		w.Write([]byte(`{"id":"/work/my-project","name":"my-project","path":"/work/my-project","status":"open"}`))
	}))
	defer srv.Close()
	p, err := New(srv.URL).CreateProject(context.Background(), "my-project")
	require.NoError(t, err)
	require.Equal(t, "/work/my-project", p.ID)
	require.Equal(t, "my-project", p.Name)
	require.Equal(t, projectstore.StatusOpen, p.Status)
}
