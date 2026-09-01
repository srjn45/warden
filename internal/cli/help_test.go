package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func executeHelp(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestRootHelpFormsAreEquivalent(t *testing.T) {
	want, err := executeHelp(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		got, err := executeHelp(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if got != want {
			t.Errorf("%v differs from help\n--- help ---\n%s\n--- got ---\n%s", args, want, got)
		}
	}
}

func TestHelpTraversalEquivalentToCommandHelp(t *testing.T) {
	for _, path := range [][]string{{"pipeline"}, {"pipeline", "show"}} {
		traversal, err := executeHelp(t, append([]string{"help"}, path...)...)
		if err != nil {
			t.Fatal(err)
		}
		flag, err := executeHelp(t, append(path, "--help")...)
		if err != nil {
			t.Fatal(err)
		}
		if traversal != flag {
			t.Errorf("help traversal differs for %v", path)
		}
	}
}

func TestHelpDoesNotRunPersistentSideEffects(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	called := false
	root.PersistentPreRun = func(*cobra.Command, []string) { called = true }
	root.SetArgs([]string{"help", "pipeline"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("root persistent pre-run ran while rendering help")
	}
}

func TestHelpUnknownPathDiagnostic(t *testing.T) {
	_, err := executeHelp(t, "help", "definitely-not-a-command")
	if err == nil || !strings.Contains(err.Error(), `unknown help path "definitely-not-a-command"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHelpAllIncludesAliasAppendix(t *testing.T) {
	got, err := executeHelp(t, "help", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Complete command tree:", "Compatibility aliases:", "warden backends ls -> warden backends list (compatibility)", "warden i -> warden repl (compatibility)"} {
		if !strings.Contains(got, want) {
			t.Errorf("help --all missing %q", want)
		}
	}
}

func TestFreshRootHelpConstructionIsIndependentAndStable(t *testing.T) {
	a, b := newRootCmd(), newRootCmd()
	if a == b || a.Commands()[0] == b.Commands()[0] {
		t.Fatal("factory reused cobra command pointers")
	}
	a.Flags().Set("tmux-native", "true")
	if got, _ := b.Flags().GetBool("tmux-native"); got {
		t.Fatal("factory reused flag storage")
	}
	var ao, bo bytes.Buffer
	a.SetOut(&ao)
	b.SetOut(&bo)
	if err := a.Help(); err != nil {
		t.Fatal(err)
	}
	if err := b.Help(); err != nil {
		t.Fatal(err)
	}
	if ao.String() != bo.String() {
		t.Fatal("repeat construction produced different help")
	}
}

func TestValidateCommandTreeDiagnostics(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	a := &cobra.Command{Use: "a"}
	b := &cobra.Command{Use: "b"}
	SetCommandHelpMetadata(a, "not-a-group", 10, "root a", "", NodeLeaf)
	SetCommandHelpMetadata(b, "run", 10, "root b", "", NodeLeaf)
	root.AddCommand(a, b)
	err := ValidateCommandTree(root)
	if err == nil || !strings.Contains(err.Error(), "unknown help group") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHelpGoldens(t *testing.T) {
	cases := map[string][]string{"root": {"help"}, "namespace": {"help", "pipeline"}, "leaf": {"help", "pipeline", "show"}}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := executeHelp(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			if name == "root" {
				got = rootHelpOutline(got)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "help_"+name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("golden mismatch\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

func rootHelpOutline(help string) string {
	if at := strings.Index(help, "Usage:\n"); at >= 0 {
		help = help[at:]
	}
	var out []string
	for _, line := range strings.Split(help, "\n") {
		if strings.HasSuffix(line, ":") || strings.HasPrefix(line, "  ") && !strings.HasPrefix(strings.TrimSpace(line), "warden") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				out = append(out, fields[0])
			}
		}
	}
	return strings.Join(out, "\n") + "\n"
}
