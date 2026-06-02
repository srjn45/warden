package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListDirsListsSubdirectories(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "alpha"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "beta"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, ".hidden"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644))

	ts := httptest.NewServer((&Server{}).router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/fs/dirs?path=" + url.QueryEscape(root))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out DirListing
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, root, out.Path)
	require.Equal(t, filepath.Dir(root), out.Parent)
	names := []string{}
	for _, e := range out.Entries {
		names = append(names, e.Name)
	}
	require.Equal(t, []string{"alpha", "beta"}, names, "subdirs only, sorted, no hidden, no files")
}

func TestListDirsRejectsNonDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))

	ts := httptest.NewServer((&Server{}).router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/fs/dirs?path=" + url.QueryEscape(f))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
