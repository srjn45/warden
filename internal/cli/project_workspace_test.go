package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

func TestProjectWorkspaceNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	projectPairs := map[string]string{
		"project memory": "memory",
		"project preset": "preset", "project preset save": "preset save", "project preset list": "preset list",
		"project prompt-template": "prompt-template", "project prompt-template save": "prompt-template save",
		"project prompt-template list": "prompt-template list",
		"project library":              "library", "project library list": "library list",
		"project plugin": "plugin", "project plugin list": "plugin list",
	}
	workspacePairs := map[string]string{
		"workspace": "worktree", "workspace list": "worktree list", "workspace prune": "prune",
		"workspace snapshot": "snapshot", "workspace snapshot create": "snapshot create",
		"workspace snapshot list": "snapshot list", "workspace snapshot restore": "snapshot restore",
		"workspace branches": "branches",
	}
	for canonical, legacy := range projectPairs {
		assertCanonicalAliasPair(t, root, canonical, legacy, false)
	}
	for canonical, legacy := range workspacePairs {
		assertCanonicalAliasPair(t, root, canonical, legacy, false)
	}
	assertCanonicalAliasPair(t, root, "project preset save", "library save-preset", false)
	assertCanonicalAliasPair(t, root, "project prompt-template save", "library save-prompt", false)
}

func TestWorkspaceCanonicalAliasDispatch(t *testing.T) {
	for _, args := range [][]string{
		{"workspace", "list", "--repo", "/r", "--json"},
		{"worktree", "list", "--repo", "/r", "--json"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			addr := stubDaemon(t, worktreeListStub)
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, `"path"`) || !strings.Contains(out, "feat") {
				t.Fatalf("unexpected output: %q", out)
			}
		})
	}
}

func worktreeListStub(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"worktrees": []lifecycle.WorktreeListing{
			{Path: "/r/.worktrees/a", Branch: "feat", Owner: "code-1", Lifecycle: "active", State: "live"},
		},
	})
}

func TestWorkspacePrunePreservesDeclineSafeguard(t *testing.T) {
	stub := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []lifecycle.PruneResult{
				{Action: lifecycle.PruneRemove, Path: "/r/.worktrees/a", Branch: "feat", State: "orphan"},
			},
		})
	}
	for _, args := range [][]string{{"workspace", "prune"}, {"prune"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			called := 0
			addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				called++
				stub(w, r)
			})
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetIn(strings.NewReader("n\n"))
			root.SetArgs(append(append([]string{}, args...), "--addr", addr, "--repo", "/r", "--config", t.TempDir()+"/none.yaml"))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if called != 1 || !strings.Contains(out.String(), "aborted") {
				t.Fatalf("decline safeguard changed: called=%d out=%q", called, out.String())
			}
		})
	}
}

func TestProjectWorkspaceProgressiveHelp(t *testing.T) {
	for name, args := range map[string][]string{
		"project":         {"help", "project"},
		"workspace":       {"help", "workspace"},
		"workspace_prune": {"help", "workspace", "prune"},
	} {
		got, err := executeHelp(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		requireGolden(t, name, got)
	}
}

func TestProjectWorkspaceFactoriesAreFresh(t *testing.T) {
	projectA, projectB := newProjectCmd(), newProjectCmd()
	if projectA == projectB || projectA.Commands()[0] == projectB.Commands()[0] {
		t.Fatal("project factory reused Cobra command pointers")
	}
	workspaceA, workspaceB := newWorkspaceCmd(), newWorkspaceCmd()
	if workspaceA == workspaceB || workspaceA.Commands()[0] == workspaceB.Commands()[0] {
		t.Fatal("workspace factory reused Cobra command pointers")
	}
	listA := findExactCommand(t, wrapRoot(workspaceA), "workspace list")
	listB := findExactCommand(t, wrapRoot(workspaceB), "workspace list")
	if err := listA.Flags().Set("repo", "/changed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := listB.Flags().GetString("repo"); got != "" {
		t.Fatalf("workspace factories share flag storage: %q", got)
	}
}

func TestWorkspaceBareListsLikeLegacyWorktree(t *testing.T) {
	addr := stubDaemon(t, worktreeListStub)
	canonical, err := runCLI(t, addr, "workspace", "--repo", "/r")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := runCLI(t, addr, "worktree", "--repo", "/r")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != legacy {
		t.Fatalf("bare workspace and worktree diverged:\nworkspace: %q\nworktree:  %q", canonical, legacy)
	}
}

func TestRetiredConflictCommandsAreRejected(t *testing.T) {
	for _, args := range [][]string{{"workspace", "conflicts"}, {"workspace", "who-is-editing"}, {"collab", "conflicts"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_, err := runCLI(t, "", args...)
			require.Error(t, err)
			require.Contains(t, err.Error(), "unknown command")
		})
	}
}
