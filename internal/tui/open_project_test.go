package tui

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeRecents is an in-memory ProjectRecents for testing the Open Project panel.
type fakeRecents struct {
	list    []projectstore.Recent
	touched []projectstore.Recent
	listErr error
}

func (f *fakeRecents) List() ([]projectstore.Recent, error) { return f.list, f.listErr }
func (f *fakeRecents) Touch(r projectstore.Recent) error {
	f.touched = append(f.touched, r)
	return nil
}

func TestOKeyOpensPanelAndLoadsRecents(t *testing.T) {
	fr := &fakeRecents{list: []projectstore.Recent{
		{Key: "github.com/org/api", Name: "api", Remote: "git@github.com:org/api.git", LastOpened: time.Now()},
	}}
	m := newListPane(&fakeAPI{}, "%9", "")
	m.recents = fr
	nm, cmd := m.Update(key("o"))
	m = nm.(controlPaneModel)
	require.Equal(t, modeOpenProject, m.mode)
	require.NotNil(t, cmd, "o dispatches a recents load")
	m = lstep(m, cmd().(recentsMsg))
	require.Len(t, m.recentList, 1)
	require.Equal(t, "api", m.recentList[0].Name)
	require.Contains(t, m.openProjectBody(), "api", "the recent project renders in the panel")
	require.Contains(t, m.openProjectBody(), "open via git")
}

func TestOpenProjectGitEntryDispatchesClone(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, key("o"))
	m = lstep(m, key("g"))
	require.Equal(t, modeOpenProjectGit, m.mode)
	m.tp.SetValue("https://github.com/org/repo.git")
	nm, cmd := m.Update(key("enter"))
	m = nm.(controlPaneModel)
	require.Equal(t, modeNormal, m.mode)
	require.NotNil(t, cmd, "enter dispatches a clone")
	require.Contains(t, m.status, "cloning")
}

func TestOpenProjectEscFromSubmodeReturnsToPanel(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m = lstep(m, key("o"))
	m = lstep(m, key("l"))
	require.Equal(t, modeOpenProjectLocal, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeOpenProject, m.mode, "esc from local returns to the panel, not normal")
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode, "esc from the panel returns to the Projects view")
}

func TestOpenResolvedProjectTouchesRecents(t *testing.T) {
	fr := &fakeRecents{}
	m := newListPane(&fakeAPI{}, "%9", "")
	m.recents = fr
	rec := projectstore.Recent{Key: "local:/work/x", Name: "x", Path: "/work/x"}
	m.openResolvedProject(projectOpenMsg{rec: rec})
	require.Len(t, fr.touched, 1, "opening a project records it in the recent list")
	require.Equal(t, "local:/work/x", fr.touched[0].Key)
}

func TestOpenResolvedProjectErrorSetsStatus(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	nm, _ := m.openResolvedProject(projectOpenMsg{err: errString("boom")})
	require.Contains(t, nm.(controlPaneModel).status, "boom")
}

func TestOrchestratorForMatchesByKey(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	orch := liveAgent("o1", "/work/api")
	orch.Role = "orchestrator"
	worker := liveAgent("w1", "/work/api")
	worker.Role = "worker"
	m.sessions = []*store.Session{worker, orch}
	m.projKeys = map[string]string{"/work/api": "local:/work/api"}
	require.Equal(t, "o1", m.orchestratorFor("local:/work/api").ID, "only the orchestrator role matches")
	require.Nil(t, m.orchestratorFor("local:/other"), "a non-matching key finds nothing")
	require.Nil(t, m.orchestratorFor(""), "an empty key finds nothing")
}

func TestProjectDisplayName(t *testing.T) {
	require.Equal(t, "repo", projectDisplayName("github.com/org/repo", "/tmp/repo"))
	require.Equal(t, "myproj", projectDisplayName("local:/home/me/myproj", "/home/me/myproj"))
	require.Equal(t, "leaf", projectDisplayName("", "/a/b/leaf"))
}

func TestRepoNameFromURL(t *testing.T) {
	require.Equal(t, "repo", repoNameFromURL("https://github.com/org/repo.git"))
	require.Equal(t, "repo", repoNameFromURL("git@github.com:org/repo.git"))
	require.Equal(t, "repo", repoNameFromURL("https://github.com/org/repo/"))
	require.Equal(t, "repo", repoNameFromURL("git@host:repo"))
}

// errString is a tiny error for status-path tests.
type errString string

func (e errString) Error() string { return string(e) }
