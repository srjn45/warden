package client

import (
	"context"
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
