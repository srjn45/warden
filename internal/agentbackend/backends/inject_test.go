package backends

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// injectCases enumerates every backend that implements agentbackend.ContextInjector
// via the shared writeRulesFile helper, paired with the rules file warden VERIFIED it
// auto-reads on startup. Codex (the pilot) keeps its own dedicated tests in
// codex_test.go; this table covers the fan-out backends with the same assertions so a
// regression in the shared helper is caught for all of them. Aider is intentionally
// absent — it has no auto-read rules file (see TestAiderNotContextInjector).
var injectCases = []struct {
	name    string
	backend agentbackend.Backend
	file    string
}{
	{"opencode", OpenCode{}, "AGENTS.md"},
	{"crush", Crush{}, "CRUSH.md"},
	{"antigravity", Antigravity{}, "AGENTS.md"},
	{"cursor", Cursor{}, "AGENTS.md"},
	{"goose", Goose{}, ".goosehints"},
}

// TestInjectImplementsContextInjector locks that each fan-out backend carries the
// optional seam (the lifecycle type-assert keys off this), unlike a flag-based backend.
func TestInjectImplementsContextInjector(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			inj, ok := tc.backend.(agentbackend.ContextInjector)
			require.True(t, ok, "%s injects context via a rules file", tc.name)
			require.NotNil(t, inj)
		})
	}
}

// TestInjectWritesBlock verifies a fresh workdir gets the agent's rules file carrying
// warden's addendum inside the delimited block.
func TestInjectWritesBlock(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, tc.backend.(agentbackend.ContextInjector).InjectContext(dir, "warden coordination hints"))

			got, err := os.ReadFile(filepath.Join(dir, tc.file))
			require.NoError(t, err)
			s := string(got)
			require.Contains(t, s, "<!-- warden:begin -->")
			require.Contains(t, s, "<!-- warden:end -->")
			require.Contains(t, s, "warden coordination hints")
		})
	}
}

// TestInjectIdempotent verifies a second call replaces the warden block in place
// rather than appending a duplicate.
func TestInjectIdempotent(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			inj := tc.backend.(agentbackend.ContextInjector)
			require.NoError(t, inj.InjectContext(dir, "first hints"))
			require.NoError(t, inj.InjectContext(dir, "second hints"))

			got, err := os.ReadFile(filepath.Join(dir, tc.file))
			require.NoError(t, err)
			s := string(got)
			require.Equal(t, 1, strings.Count(s, "<!-- warden:begin -->"), "no duplicate warden block")
			require.Equal(t, 1, strings.Count(s, "<!-- warden:end -->"))
			require.Contains(t, s, "second hints")
			require.NotContains(t, s, "first hints", "stale warden block replaced in place")
		})
	}
}

// TestInjectPreservesUserFile verifies a user's pre-existing rules file content
// survives: only the warden block is added/refreshed around it.
func TestInjectPreservesUserFile(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.file)
			require.NoError(t, os.WriteFile(path, []byte("# My project rules\nAlways run the linter.\n"), 0o644))

			inj := tc.backend.(agentbackend.ContextInjector)
			require.NoError(t, inj.InjectContext(dir, "warden hints"))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			s := string(got)
			require.Contains(t, s, "# My project rules", "user content preserved")
			require.Contains(t, s, "Always run the linter.")
			require.Contains(t, s, "warden hints")

			// A second inject still preserves the user content and doesn't duplicate the block.
			require.NoError(t, inj.InjectContext(dir, "warden hints v2"))
			got, err = os.ReadFile(path)
			require.NoError(t, err)
			s = string(got)
			require.Contains(t, s, "# My project rules")
			require.Equal(t, 1, strings.Count(s, "<!-- warden:begin -->"))
			require.Contains(t, s, "warden hints v2")
			require.NotContains(t, s, "warden hints\n", "old warden text replaced")
		})
	}
}

// TestInjectNoOps verifies an empty workdir or empty text writes nothing.
func TestInjectNoOps(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			inj := tc.backend.(agentbackend.ContextInjector)
			require.NoError(t, inj.InjectContext("", "hints"))

			dir := t.TempDir()
			require.NoError(t, inj.InjectContext(dir, "   \n  "))
			_, err := os.Stat(filepath.Join(dir, tc.file))
			require.True(t, os.IsNotExist(err), "empty text writes no file")
		})
	}
}

// TestInjectGitExclude verifies the dropped rules file is added to the repo's
// info/exclude (so it never lands in the agent's diff), idempotently.
func TestInjectGitExclude(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o755))

			inj := tc.backend.(agentbackend.ContextInjector)
			require.NoError(t, inj.InjectContext(dir, "hints"))
			require.NoError(t, inj.InjectContext(dir, "hints again"))

			excl, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(excl), tc.file), "excluded once, not duplicated")
		})
	}
}

// TestInjectGitExcludeLinkedWorktree verifies a linked worktree (.git is a file
// pointing at a per-worktree gitdir whose commondir names the shared .git) resolves
// its exclude through the shared commondir — the path warden agents actually run in.
func TestInjectGitExcludeLinkedWorktree(t *testing.T) {
	for _, tc := range injectCases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			// Shared repo (commondir) and a per-worktree gitdir under it.
			common := filepath.Join(root, "mainrepo", ".git")
			require.NoError(t, os.MkdirAll(filepath.Join(common, "info"), 0o755))
			gitdir := filepath.Join(common, "worktrees", "wt1")
			require.NoError(t, os.MkdirAll(gitdir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o644))

			work := filepath.Join(root, "wt1")
			require.NoError(t, os.MkdirAll(work, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644))

			require.NoError(t, tc.backend.(agentbackend.ContextInjector).InjectContext(work, "hints"))

			excl, err := os.ReadFile(filepath.Join(common, "info", "exclude"))
			require.NoError(t, err)
			require.Contains(t, string(excl), tc.file, "excluded via the shared commondir")
		})
	}
}

// TestAiderNotContextInjector documents the skip: Aider has no rules file it auto-reads
// on a bare warden launch (CONVENTIONS.md needs --read / .aider.conf.yml), so it must
// NOT implement agentbackend.ContextInjector — warden injects nothing rather than drop
// a dead file Aider ignores. See aider.SystemPromptFlag and docs/agent-backends/aider.md.
func TestAiderNotContextInjector(t *testing.T) {
	_, ok := agentbackend.Backend(Aider{}).(agentbackend.ContextInjector)
	require.False(t, ok, "Aider does not auto-read a rules file, so it injects nothing")
}
