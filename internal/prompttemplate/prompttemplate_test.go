package prompttemplate

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
	_, ok := s.Get("nope")
	require.False(t, ok)
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "prompt-templates.yaml") // dir created on Save
	s, err := Load(path)
	require.NoError(t, err)

	want := Template{Prompt: "Fix the bug in {{FILE}} for {{TICKET}}", Vars: []string{"FILE", "TICKET"}}
	s.Set("bugfix", want)
	require.NoError(t, s.Save(path))
	require.FileExists(t, path)

	got, err := Load(path)
	require.NoError(t, err)
	tpl, ok := got.Get("bugfix")
	require.True(t, ok)
	require.Equal(t, want, tpl)
}

func TestSetOverwritesAndNamesSorted(t *testing.T) {
	s := &Store{}
	s.Set("zeta", Template{Prompt: "z"})
	s.Set("alpha", Template{Prompt: "a"})
	s.Set("zeta", Template{Prompt: "z2"}) // overwrite
	require.Equal(t, []string{"alpha", "zeta"}, s.Names())
	tpl, _ := s.Get("zeta")
	require.Equal(t, "z2", tpl.Prompt)
}

func TestLoadCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt-templates.yaml")
	require.NoError(t, os.WriteFile(path, []byte("templates: [not-a-map\n"), 0o600))
	_, err := Load(path)
	require.Error(t, err)
}

func TestPlaceholdersDistinctInOrder(t *testing.T) {
	got := Placeholders("{{B}} then {{A}} then {{ B }} again and {{C}}")
	require.Equal(t, []string{"B", "A", "C"}, got)
	require.Nil(t, Placeholders("no placeholders here"))
}

func TestResolveSubstitutesAllVars(t *testing.T) {
	tpl := Template{Prompt: "Refactor {{FILE}} to use {{LIB}}", Vars: []string{"FILE", "LIB"}}
	out, err := tpl.Resolve(map[string]string{"FILE": "foo.go", "LIB": "errgroup"})
	require.NoError(t, err)
	require.Equal(t, "Refactor foo.go to use errgroup", out)
}

func TestResolveTrimsBracketSpacing(t *testing.T) {
	tpl := Template{Prompt: "Hello {{ NAME }}!", Vars: []string{"NAME"}}
	out, err := tpl.Resolve(map[string]string{"NAME": "world"})
	require.NoError(t, err)
	require.Equal(t, "Hello world!", out)
}

func TestResolveMissingVarErrors(t *testing.T) {
	tpl := Template{Prompt: "Fix {{FILE}} for {{TICKET}}", Vars: []string{"FILE", "TICKET"}}
	_, err := tpl.Resolve(map[string]string{"FILE": "foo.go"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TICKET")
	require.Contains(t, err.Error(), "--set")
}

func TestResolveUnknownVarErrors(t *testing.T) {
	tpl := Template{Prompt: "Fix {{FILE}}", Vars: []string{"FILE"}}
	_, err := tpl.Resolve(map[string]string{"FILE": "foo.go", "TYPO": "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TYPO")
}

// TestResolveFallsBackToPlaceholdersWhenVarsUndeclared covers a template stored
// without an explicit Vars list (declarations are derived from the body).
func TestResolveFallsBackToPlaceholdersWhenVarsUndeclared(t *testing.T) {
	tpl := Template{Prompt: "Build {{TARGET}}"}
	out, err := tpl.Resolve(map[string]string{"TARGET": "site"})
	require.NoError(t, err)
	require.Equal(t, "Build site", out)

	_, err = tpl.Resolve(map[string]string{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "TARGET")
}

// TestResolveValuesNotRescanned ensures a value that itself looks like a
// placeholder is inserted verbatim, not expanded again.
func TestResolveValuesNotRescanned(t *testing.T) {
	tpl := Template{Prompt: "echo {{X}}", Vars: []string{"X"}}
	out, err := tpl.Resolve(map[string]string{"X": "{{Y}}"})
	require.NoError(t, err)
	require.Equal(t, "echo {{Y}}", out)
}
