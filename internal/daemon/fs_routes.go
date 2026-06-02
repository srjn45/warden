package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dirEntry is one subdirectory in a DirListing.
type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DirListing is the body of GET /fs/dirs: the resolved directory, its parent
// (empty at the filesystem root), and its immediate subdirectories.
type DirListing struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Entries []dirEntry `json:"entries"`
}

// handleListDirs lists the immediate subdirectories of ?path= (defaulting to the
// user's home directory). It powers the web "new agent" directory picker, which
// cannot use a native folder dialog (browsers hide absolute paths).
func (s *Server) handleListDirs(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "no home directory: "+err.Error())
			return
		}
		path = home
	}
	path = filepath.Clean(path)
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "not a directory: "+path)
		return
	}
	items, err := os.ReadDir(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read directory: "+err.Error())
		return
	}
	entries := []dirEntry{}
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		entries = append(entries, dirEntry{Name: it.Name(), Path: filepath.Join(path, it.Name())})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // already at the filesystem root
	}
	writeJSON(w, http.StatusOK, DirListing{Path: path, Parent: parent, Entries: entries})
}
