package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func runVersion(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"version"}, args...))
	require.NoError(t, root.Execute())
	return out.String()
}

func TestVersionCommandText(t *testing.T) {
	out := runVersion(t)
	require.Contains(t, out, "warden "+version)
	require.Contains(t, out, "Commit:")
	require.Contains(t, out, "Built:")
	require.Contains(t, out, "Go: ")
	require.Contains(t, out, "Platform:")
}

func TestVersionCommandJSON(t *testing.T) {
	out := runVersion(t, "--json")
	var bi buildInfo
	require.NoError(t, json.Unmarshal([]byte(out), &bi))
	require.Equal(t, version, bi.Version)
	require.NotEmpty(t, bi.Commit)
	require.NotEmpty(t, bi.Date)
	require.NotEmpty(t, bi.GoVersion)
	require.Contains(t, bi.Platform, "/")
}

// currentBuildInfo fills empty commit/date with "unknown" rather than leaving
// them blank, so output is never ambiguous.
func TestCurrentBuildInfoFallback(t *testing.T) {
	bi := currentBuildInfo()
	require.NotEmpty(t, bi.Commit)
	require.NotEmpty(t, bi.Date)
}

// `warden --version` prints the same detailed block as `warden version`.
func TestVersionFlagUsesBuildInfo(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	require.NoError(t, root.Execute())
	require.True(t, strings.HasPrefix(out.String(), "warden "+version),
		"--version should print the build-info block, got: %q", out.String())
	require.Contains(t, out.String(), "Go: ")
}
