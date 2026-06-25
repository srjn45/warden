package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/plugin"
)

func TestFormatPluginListDisabledEmpty(t *testing.T) {
	out := formatPluginList(false, nil)
	require.Contains(t, out, "disabled")
	require.Contains(t, out, "no plugins registered")
}

func TestFormatPluginListEnabledEmpty(t *testing.T) {
	out := formatPluginList(true, nil)
	require.Contains(t, out, "ENABLED")
	require.Contains(t, out, "no plugins registered")
}

func TestFormatPluginListWithPlugins(t *testing.T) {
	specs := []plugin.Spec{{
		Name:   "notifier",
		Path:   "/bin/notifier",
		Events: []string{"post-commit", "post-spawn"},
		TaskTypes: []plugin.TaskTypeSpec{
			{Name: "lint-bot", Worktree: true},
			{Name: "scratch", Worktree: false},
		},
	}}
	out := formatPluginList(true, specs)
	require.Contains(t, out, "notifier")
	require.Contains(t, out, "/bin/notifier")
	require.Contains(t, out, "lint-bot (worktree)")
	require.Contains(t, out, "scratch (in-repo)")
	require.Contains(t, out, "post-spawn")
	require.Contains(t, out, "post-commit")
	require.NotContains(t, out, "config errors")
}

func TestFormatPluginListSurfacesConfigErrors(t *testing.T) {
	// Duplicate name is a Load error; the listing must flag it but still render.
	specs := []plugin.Spec{
		{Name: "dup", Path: "/a"},
		{Name: "dup", Path: "/b"},
	}
	out := formatPluginList(true, specs)
	require.Contains(t, out, "config errors")
	require.Contains(t, out, "dup")
}

func TestFormatEventsCanonicalOrder(t *testing.T) {
	// Input is out of order; output follows AllEvents order.
	got := formatEvents([]string{"post-check", "pre-spawn", "post-commit"})
	require.Equal(t, "pre-spawn, post-commit, post-check", got)
}

func TestFormatEventsSurfacesUnknownLast(t *testing.T) {
	got := formatEvents([]string{"on-tuesday", "pre-spawn"})
	require.True(t, strings.HasPrefix(got, "pre-spawn"))
	require.Contains(t, got, "on-tuesday")
}

func TestFormatEventsEmpty(t *testing.T) {
	require.Equal(t, "(none)", formatEvents(nil))
}
