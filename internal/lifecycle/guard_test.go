package lifecycle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// settingsMatchers unmarshals a generated settings doc into (matcher → command).
func settingsMatchers(t *testing.T, raw string) map[string]string {
	t.Helper()
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	out := map[string]string{}
	for _, m := range doc.Hooks.PreToolUse {
		require.Len(t, m.Hooks, 1)
		require.Equal(t, "command", m.Hooks[0].Type)
		out[m.Matcher] = m.Hooks[0].Command
	}
	return out
}

func TestGuardSettingsJSON(t *testing.T) {
	// Both hooks on → both matchers present, each pointing at its warden subcommand.
	m := settingsMatchers(t, guardSettingsJSON("/usr/local/bin/warden", true, true))
	require.Len(t, m, 2)
	require.Contains(t, m["Edit|Write|MultiEdit|NotebookEdit"], "hook guard")
	require.Contains(t, m["Bash"], "hook git-guard")
	require.Contains(t, m["Bash"], "warden")
}

func TestGuardSettingsJSONOnlyGitRedirect(t *testing.T) {
	// Isolation off, git redirect on → just the Bash matcher.
	m := settingsMatchers(t, guardSettingsJSON("/usr/local/bin/warden", false, true))
	require.Len(t, m, 1)
	require.Contains(t, m["Bash"], "hook git-guard")
}

func TestGuardSettingsJSONBothOffIsEmpty(t *testing.T) {
	require.Empty(t, guardSettingsJSON("/usr/local/bin/warden", false, false))
}

func TestGuardSettingsFlagDisabledOrUnconfigured(t *testing.T) {
	// Unconfigured (no SettingsDir/WardenBin) → no flag.
	require.Empty(t, New(&FakeRunner{}, &FakeConfig{}).guardSettingsFlag("a"))

	// Configured but every PreToolUse hook disabled in config → no flag, no file.
	dir := t.TempDir()
	lc := New(&FakeRunner{}, &FakeConfig{IsolationGuardOff: true, GitRedirectOff: true})
	lc.SettingsDir, lc.WardenBin = dir, "/usr/bin/warden"
	require.Empty(t, lc.guardSettingsFlag("a"))
	_, err := os.Stat(filepath.Join(dir, "a.json"))
	require.True(t, os.IsNotExist(err), "no settings file when all hooks disabled")
}

func TestGuardSettingsFlagWritesFile(t *testing.T) {
	dir := t.TempDir()
	lc := New(&FakeRunner{}, &FakeConfig{})
	lc.SettingsDir, lc.WardenBin = dir, "/usr/bin/warden"

	frag := lc.guardSettingsFlag("code-1")
	path := filepath.Join(dir, "code-1.json")
	require.Equal(t, " --settings "+shellQuoteArg(path), frag)

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(b), "PreToolUse")
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

// sendKeysLaunch returns the launch string from the agent's `tmux send-keys`.
func sendKeysLaunch(t *testing.T, fr *FakeRunner, id string) string {
	t.Helper()
	for _, argv := range fr.calledArgs() {
		if len(argv) >= 5 && argv[0] == "tmux" && argv[1] == "send-keys" && argv[3] == id {
			return argv[4]
		}
	}
	t.Fatalf("no send-keys launch found for %s", id)
	return ""
}

func TestSpawnIsolatedAgentGetsSettingsFlag(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr, &FakeConfig{})
	lc.SettingsDir, lc.WardenBin = t.TempDir(), "/usr/bin/warden"
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypeDebugCI, Repo: "/repo"})
	require.NoError(t, err)
	require.Contains(t, sendKeysLaunch(t, fr, s.ID), "--settings",
		"an isolated write-agent must launch with the guard --settings file")
}

func TestSpawnAgentSkipsSettingsFlagWhenAllHooksOff(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr, &FakeConfig{IsolationGuardOff: true, GitRedirectOff: true})
	lc.SettingsDir, lc.WardenBin = t.TempDir(), "/usr/bin/warden"
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypeDebugCI, Repo: "/repo", InRepo: true})
	require.NoError(t, err)
	require.NotContains(t, sendKeysLaunch(t, fr, s.ID), "--settings")
}

func TestSpawnGitRedirectOnlyStillGetsSettingsFlag(t *testing.T) {
	// Isolation guard off but git redirect on (the default) → a settings file is
	// still written, carrying just the Bash git-guard hook.
	fr := &FakeRunner{}
	lc := New(fr, &FakeConfig{IsolationGuardOff: true})
	lc.SettingsDir, lc.WardenBin = t.TempDir(), "/usr/bin/warden"
	s, err := lc.Spawn(context.Background(), SpawnRequest{Type: store.TypeDebugCI, Repo: "/repo"})
	require.NoError(t, err)
	require.Contains(t, sendKeysLaunch(t, fr, s.ID), "--settings")
}
