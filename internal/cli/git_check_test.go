package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestGitCheckNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	gitPairs := map[string]string{
		"git commit": "commit", "git push": "push", "git sync": "sync", "git review": "review",
		"git guard": "hook git-guard",
	}
	checkPairs := map[string]string{
		"check guard": "hook check-guard", "check boundary": "hook guard", "check root-guard": "hook root-guard",
	}
	permanent := map[string]bool{"commit": true, "push": true, "sync": true, "check": true}
	for canonical, legacy := range gitPairs {
		assertCanonicalAliasPair(t, root, canonical, legacy, permanent[legacy])
	}
	for canonical, legacy := range checkPairs {
		assertCanonicalAliasPair(t, root, canonical, legacy, false)
	}
	reviewCmd := findExactCommand(t, root, "review")
	if !reviewCmd.Hidden {
		t.Fatal("legacy review must be hidden")
	}
	if got := reviewCmd.Annotations[AnnotationCanonicalPath]; got != "warden git review" {
		t.Fatalf("review canonical=%q", got)
	}
}

func assertCanonicalAliasPair(t *testing.T, root *cobra.Command, canonical, legacy string, permanent bool) {
	t.Helper()
	canonicalCmd := findExactCommand(t, root, canonical)
	legacyCmd := findExactCommand(t, root, legacy)
	if legacyCmd.Hidden == permanent {
		t.Errorf("legacy %q hidden=%v, permanent=%v", legacy, legacyCmd.Hidden, permanent)
	}
	wantKind := AliasCompatibility
	if permanent {
		wantKind = AliasPermanentShortcut
	}
	if got := legacyCmd.Annotations[AnnotationAliasKind]; got != wantKind {
		t.Errorf("legacy %q alias kind=%q, want %q", legacy, got, wantKind)
	}
	if got, want := legacyCmd.Annotations[AnnotationCanonicalPath], "warden "+canonical; got != want {
		t.Errorf("legacy %q canonical=%q, want %q", legacy, got, want)
	}
	if got, want := commandFlagSignature(canonicalCmd), commandFlagSignature(legacyCmd); !reflect.DeepEqual(got, want) {
		t.Errorf("%s flags differ from %s: %v != %v", canonical, legacy, got, want)
	}
}

func TestGitCheckCanonicalAliasDispatchAndJSON(t *testing.T) {
	for _, args := range [][]string{
		{"git", "commit", "-m", "x", "--json"}, {"commit", "-m", "x", "--json"},
		{"check", "run", "--json"}, {"check", "--json"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			method := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, map[string]string{
				"POST /api/v1/git/commit": `{"committed":true,"sha":"abc","branch":"feat","files":["a.go"]}`,
				"POST /api/v1/check":      `{"passed":true,"checks":[]}`,
			}, method, nil))
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &got))
			if strings.Contains(strings.Join(args, " "), "commit") {
				require.Equal(t, http.MethodPost, method["/api/v1/git/commit"])
				require.Equal(t, "abc", got["sha"])
			} else {
				require.Equal(t, http.MethodPost, method["/api/v1/check"])
				require.Equal(t, true, got["passed"])
			}
		})
	}
}

func TestGitCheckHookProtocolEquivalence(t *testing.T) {
	stdin := bashHookInput(t, "go test ./...", t.TempDir())
	pairs := map[string]string{
		"git guard": "hook git-guard", "check guard": "hook check-guard",
		"check boundary": "hook guard", "check root-guard": "hook root-guard",
	}
	for canonical, legacy := range pairs {
		t.Run(canonical, func(t *testing.T) {
			if strings.Contains(canonical, "guard") && canonical != "check guard" && canonical != "git guard" {
				stdin = `{"tool_name":"Edit","tool_input":{"file_path":"/tmp/x"},"cwd":"` + t.TempDir() + `"}`
			}
			if canonical == "check boundary" {
				addr := stubDaemon(t, routedDaemon(t, map[string]string{
					"POST /api/v1/hooks/guard": `{"decision":"deny","reason":"blocked"}`,
				}, map[string]string{}, nil))
				canonicalOut := runHookPath(t, addr, strings.Split(canonical, " "), stdin)
				legacyOut := runHookPath(t, addr, strings.Split(legacy, " "), stdin)
				require.Equal(t, canonicalOut, legacyOut)
				return
			}
			if canonical == "check root-guard" {
				stdin = `{"tool_name":"Edit","tool_input":{"file_path":"/tmp/x"},"cwd":"` + t.TempDir() + `"}`
			}
			if canonical == "git guard" {
				stdin = bashHookInput(t, "git commit -m x", t.TempDir())
			}
			canonicalOut := runHookPath(t, "", strings.Split(canonical, " "), stdin)
			legacyOut := runHookPath(t, "", strings.Split(legacy, " "), stdin)
			require.Equal(t, canonicalOut, legacyOut)
		})
	}
}

func runHookPath(t *testing.T, addr string, path []string, stdin string) string {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	args := append(path, "--config", t.TempDir()+"/none.yaml")
	if addr != "" {
		args = append(args, "--addr", addr)
	}
	root.SetArgs(args)
	require.NoError(t, root.Execute())
	return out.String()
}

func TestGitCheckProgressiveHelp(t *testing.T) {
	gitNS, err := executeHelp(t, "help", "git")
	require.NoError(t, err)
	gitLeaf, err := executeHelp(t, "help", "git", "commit")
	require.NoError(t, err)
	checkNS, err := executeHelp(t, "help", "check")
	require.NoError(t, err)
	checkLeaf, err := executeHelp(t, "help", "check", "run")
	require.NoError(t, err)
	if !strings.Contains(gitNS, "warden rails") || !strings.Contains(gitNS, "commit") {
		t.Fatalf("git namespace help missing domain guidance: %s", gitNS)
	}
	if !strings.Contains(gitLeaf, "--message") || strings.Contains(gitLeaf, "git push") {
		t.Fatalf("git commit help is not focused: %s", gitLeaf)
	}
	if !strings.Contains(checkNS, ".warden/check.yml") || !strings.Contains(checkNS, "run") {
		t.Fatalf("check namespace help missing domain guidance: %s", checkNS)
	}
	if !strings.Contains(checkLeaf, "--json") || strings.Contains(checkLeaf, "boundary") {
		t.Fatalf("check run help is not focused: %s", checkLeaf)
	}
	for name, got := range map[string]string{
		"git": gitNS, "git_commit": gitLeaf, "check": checkNS, "check_run": checkLeaf,
	} {
		want, err := os.ReadFile(filepath.Join("testdata", "help_"+name+".golden"))
		require.NoError(t, err)
		require.Equal(t, string(want), got, name)
	}
}

func TestGitCheckFactoriesAreFreshAndErrorsMatch(t *testing.T) {
	gitA, gitB := newGitCmd(), newGitCmd()
	if gitA == gitB || gitA.Commands()[0] == gitB.Commands()[0] {
		t.Fatal("git factory reused Cobra command pointers")
	}
	checkA, checkB := newCheckNamespaceCmd(), newCheckNamespaceCmd()
	if checkA == checkB || checkA.Commands()[0] == checkB.Commands()[0] {
		t.Fatal("check factory reused Cobra command pointers")
	}
	commitA := findExactCommand(t, wrapRoot(gitA), "git commit")
	commitB := findExactCommand(t, wrapRoot(gitB), "git commit")
	require.NoError(t, commitA.Flags().Set("message", "changed"))
	got, _ := commitB.Flags().GetString("message")
	if got != "" {
		t.Fatalf("git factories share flag storage: %q", got)
	}
}

func TestCheckShortcutNamespaceSharesRunFlags(t *testing.T) {
	root := newRootCmd()
	ns := findExactCommand(t, root, "check")
	run := findExactCommand(t, root, "check run")
	if got, want := commandFlagSignature(ns), commandFlagSignature(run); !reflect.DeepEqual(got, want) {
		t.Fatalf("check shortcut/run flags differ: %v != %v", got, want)
	}
}

func TestGitCheckValidateTree(t *testing.T) {
	require.NoError(t, ValidateCommandTree(newRootCmd()))
}
