package tui

import (
	"testing"

	"github.com/srjn45/warden/internal/projectstore"
	"github.com/stretchr/testify/require"
)

func TestSpawnCmdUsesGivenCwd(t *testing.T) {
	f := &fakeAPI{}
	msg := spawnCmd(f, "do the thing", "my-agent", "/work/api", "reviewer", "aider", false)()
	done, ok := msg.(spawnDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.NotNil(t, f.spawned)
	require.Equal(t, "/work/api", f.spawned.Cwd)
	require.Equal(t, "do the thing", f.spawned.Prompt)
	require.Equal(t, "my-agent", f.spawned.Name)
	require.Equal(t, "reviewer", f.spawned.Role)
	require.Equal(t, "aider", f.spawned.Backend)
}

// TestCreateProjectCmd proves createProjectCmd calls the API's CreateProject
// method with the given name and returns an openProjectMsg with the result.
func TestCreateProjectCmd(t *testing.T) {
	f := &fakeAPI{
		createdProj: projectstore.Project{
			ID: "/work/my-project", Name: "my-project",
			Path: "/work/my-project", Status: projectstore.StatusOpen,
		},
	}
	msg := createProjectCmd(f, "my-project")()
	done, ok := msg.(openProjectMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, "my-project", f.createdName)
	require.Equal(t, "/work/my-project", done.proj.ID)
	require.Equal(t, "my-project", done.proj.Name)
	require.Equal(t, projectstore.StatusOpen, done.proj.Status)
}

// TestOpenLocalProjectCmd proves openLocalProjectCmd calls OpenLocalProject.
func TestOpenLocalProjectCmd(t *testing.T) {
	f := &fakeAPI{
		openedLocalProj: projectstore.Project{
			ID: "/repos/alpha", Name: "alpha",
			Path: "/repos/alpha", Status: projectstore.StatusOpen,
		},
	}
	msg := openLocalProjectCmd(f, "/repos/alpha")()
	done, ok := msg.(openProjectMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, "/repos/alpha", f.openedLocalPath)
	require.Equal(t, "", f.openedLocalName, "name is always empty — daemon defaults to dir basename")
	require.Equal(t, "/repos/alpha", done.proj.ID)
}

// TestOpenRemoteProjectCmd proves openRemoteProjectCmd calls OpenRemoteProject.
func TestOpenRemoteProjectCmd(t *testing.T) {
	f := &fakeAPI{
		openedRemoteProj: projectstore.Project{
			ID: "/work/widgets", Name: "widgets",
			Path: "/work/widgets", Status: projectstore.StatusOpen,
		},
	}
	msg := openRemoteProjectCmd(f, "https://github.com/acme/widgets")()
	done, ok := msg.(openProjectMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.Equal(t, "https://github.com/acme/widgets", f.openedRemoteURL)
	require.Equal(t, "/work/widgets", done.proj.ID)
}
