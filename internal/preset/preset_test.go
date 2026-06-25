package preset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)
	require.Empty(t, s.Names())
	// Get on an empty store is safe and reports absence.
	_, ok := s.Get("nope")
	require.False(t, ok)
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "presets.yaml") // dir created on Save
	s, err := Load(path)
	require.NoError(t, err)

	want := Preset{Type: "pr-review", Model: "opus", PermissionMode: "acceptEdits", AutoRestart: true}
	s.Set("code-review", want)
	require.NoError(t, s.Save(path))
	require.FileExists(t, path)

	got, err := Load(path)
	require.NoError(t, err)
	p, ok := got.Get("code-review")
	require.True(t, ok)
	require.Equal(t, want, p)
}

func TestSetOverwritesAndNamesSorted(t *testing.T) {
	s := &Store{}
	s.Set("zeta", Preset{Type: "spike"})
	s.Set("alpha", Preset{Type: "docs"})
	s.Set("zeta", Preset{Type: "analysis"}) // overwrite
	require.Equal(t, []string{"alpha", "zeta"}, s.Names())
	p, _ := s.Get("zeta")
	require.Equal(t, "analysis", p.Type)
}

func TestLoadCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.yaml")
	require.NoError(t, os.WriteFile(path, []byte("presets: [not-a-map\n"), 0o600))
	_, err := Load(path)
	require.Error(t, err)
}
