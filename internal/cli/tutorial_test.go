package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTutorialMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	marker := tutorialMarkerPath(dir)
	require.Equal(t, filepath.Join(dir, tutorialMarker), marker)

	// Fresh dir: not done.
	require.False(t, tutorialDone(marker), "no marker yet")

	// Write → done, and the file carries a human-readable note.
	require.NoError(t, writeTutorialMarker(marker))
	require.True(t, tutorialDone(marker), "marker present after write")
	body, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Contains(t, string(body), "warden tutorial completed")

	// Reset → not done again.
	require.NoError(t, resetTutorialMarker(marker))
	require.False(t, tutorialDone(marker), "marker gone after reset")

	// Reset is idempotent: a missing marker is not an error.
	require.NoError(t, resetTutorialMarker(marker), "reset on missing marker is a no-op success")
}

func TestWriteTutorialMarkerCreatesDataDir(t *testing.T) {
	// data_dir may not exist yet (tutorial can run before the daemon).
	dir := filepath.Join(t.TempDir(), "fresh", "warden")
	marker := tutorialMarkerPath(dir)
	require.NoError(t, writeTutorialMarker(marker))
	require.True(t, tutorialDone(marker))
}

func TestShouldHintTutorial(t *testing.T) {
	// The one happy path: fresh (no marker) + TTY + gate on + a normal command.
	require.True(t, shouldHintTutorial(false, true, true, "ls"))

	// Every suppression condition independently silences the hint.
	require.False(t, shouldHintTutorial(true, true, true, "ls"), "marker present ⇒ no hint")
	require.False(t, shouldHintTutorial(false, false, true, "ls"), "non-TTY ⇒ no hint")
	require.False(t, shouldHintTutorial(false, true, false, "ls"), "gate off ⇒ no hint")

	// Machine / full-screen / self-referential commands never carry the hint,
	// even when fresh + TTY + gate on.
	for _, name := range []string{"warden", "tui", "tutorial", "daemon", "mcp", "hook", "guard", "completion", "repl"} {
		require.False(t, shouldHintTutorial(false, true, true, name), "%s must be suppressed", name)
	}
}

func TestTutorialStepsCoverCoreLoop(t *testing.T) {
	var buf bytes.Buffer
	renderTutorial(&buf, tutorialSteps())
	got := buf.String()
	// The walkthrough must name the core loop verbs and the two richer surfaces.
	for _, want := range []string{"wd start", "wd ls", "wd attach", "wd send", "wd done", "wd tui"} {
		require.Contains(t, got, want, "tutorial should show %q", want)
	}
}

func TestTutorialSkipWritesMarkerWithoutSteps(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.yaml"
	// Point data_dir at the temp dir so the marker lands there.
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+dir+"\n"), 0o644))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"tutorial", "--skip", "--config", cfg})
	require.NoError(t, root.Execute())

	// Marker is written, but the walkthrough body was NOT printed.
	require.True(t, tutorialDone(tutorialMarkerPath(dir)), "--skip writes the marker")
	require.Contains(t, out.String(), "tutorial skipped")
	require.NotContains(t, out.String(), "spawn → watch → talk", "--skip must not run the steps")
}

func TestTutorialRunWritesMarkerAndSteps(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.yaml"
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+dir+"\n"), 0o644))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"tutorial", "--config", cfg})
	require.NoError(t, root.Execute())

	require.True(t, tutorialDone(tutorialMarkerPath(dir)), "running to completion writes the marker")
	require.Contains(t, out.String(), "Welcome to warden")
	require.Contains(t, out.String(), "tutorial complete")
}

func TestTutorialResetRemovesMarker(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.yaml"
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+dir+"\n"), 0o644))
	marker := tutorialMarkerPath(dir)
	require.NoError(t, writeTutorialMarker(marker))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"tutorial", "--reset", "--config", cfg})
	require.NoError(t, root.Execute())

	require.False(t, tutorialDone(marker), "--reset deletes the marker")
	require.Contains(t, out.String(), "tutorial reset")
}

func TestTutorialResetAndSkipConflict(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.yaml"
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+dir+"\n"), 0o644))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"tutorial", "--reset", "--skip", "--config", cfg})
	err := root.Execute()
	require.Error(t, err, "--reset and --skip together is an error")
	require.True(t, strings.Contains(err.Error(), "mutually exclusive"))
}

// TestMaybeHintTutorialNonTTY pins the auto-prompt's safety contract: with a
// non-TTY writer (a buffer, as in all CLI tests) it never emits, regardless of
// marker state. This is why the existing runCLI-based tests stay hint-free.
func TestMaybeHintTutorialNonTTY(t *testing.T) {
	dir := t.TempDir()
	cfg := dir + "/config.yaml"
	require.NoError(t, os.WriteFile(cfg, []byte("data_dir: "+dir+"\n"), 0o644))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	// `version` is a harmless non-suppressed command; with a buffer (non-TTY)
	// the pre-run hint must stay silent.
	root.SetArgs([]string{"version", "--config", cfg})
	require.NoError(t, root.Execute())
	require.NotContains(t, out.String(), "wd tutorial", "no hint when stdout is not a TTY")
}
