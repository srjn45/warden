package terminalcontract

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goSources returns the repo-relative non-test Go files under internal/ and cmd/.
func goSources(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for _, top := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				t.Fatal(rerr)
			}
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
	}
	return out
}

// Every plain-terminal launch ends in lifecycle.RestoreTerminal (spec §2
// finding 1 / P07): a new caller must be added here AND justified in the spec,
// so no path can start a shell outside the idempotent, serialized funnel.
func TestTerminalLaunchFunnel(t *testing.T) {
	allowed := map[string]bool{
		"internal/lifecycle/lifecycle.go":         true, // definition + SpawnTerminal
		"internal/daemon/terminal_lifecycle.go":   true, // restoreTerminalRecord, spawnTerminal
		"internal/daemon/terminal_poller_deps.go": true, // watcher restore (P13)
		"internal/daemon/pty_runtime.go":          true, // startup recovery (P12)
		"internal/daemon/lifecycle_adapter.go":    true, // interface adapter
		"internal/daemon/api.go":                  true, // Lifecycle interface
	}
	for rel, src := range goSources(t) {
		if strings.Contains(src, "RestoreTerminal(") && !allowed[rel] && !strings.HasPrefix(rel, "internal/terminalcontract/") {
			t.Errorf("%s calls RestoreTerminal: terminals launch only through the funnel; add it to the spec's path matrix and this allowlist", rel)
		}
	}
	// The direct-PTY terminal host is wired exactly once.
	for rel, src := range goSources(t) {
		if strings.HasPrefix(rel, "internal/terminalcontract/") {
			continue
		}
		if strings.Contains(src, "TerminalHost()") && rel != "internal/ptyhost/manager.go" && rel != "internal/cli/daemon.go" {
			t.Errorf("%s uses the PTY TerminalHost: only the daemon wiring may", rel)
		}
	}
}

// F-KIND-structural: no terminal-side source may import the packages that
// classify output or decide on AI prompts. Shell output never reaches them.
func TestTerminalPathsDoNotImportDecisionPackages(t *testing.T) {
	banned := []string{"internal/promptengine", "internal/decisionrouter", "internal/stateclassifier", "internal/brain", "internal/fastbrain"}
	files := []string{
		"internal/terminalstore/store.go",
		"internal/poller/terminal_watcher.go",
		"internal/daemon/terminal_lifecycle.go",
		"internal/daemon/terminal_poller_deps.go",
	}
	srcs := goSources(t)
	for _, f := range files {
		src, ok := srcs[f]
		if !ok {
			t.Errorf("%s missing; update the list", f)
			continue
		}
		for _, b := range banned {
			if strings.Contains(src, "warden/"+b+"\"") {
				t.Errorf("%s imports %s: a plain terminal path must not reach AI decision code", f, b)
			}
		}
	}
}
