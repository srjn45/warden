package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

var canonicalNamespaces = []string{
	"agent", "pipeline", "autopilot", "schedule",
	"project", "workspace", "git", "check",
	"context", "message", "approval",
	"inspect", "backend", "usage", "config",
	"daemon", "completion",
}

func TestRootHelpOffersExactlyTheApprovedSurface(t *testing.T) {
	t.Parallel()

	want := map[string]bool{}
	for _, name := range canonicalNamespaces {
		want[name] = true
	}
	for _, name := range []string{"login", "setup", "tutorial", "doctor", "factory-reset", "tui", "version"} {
		want[name] = true
	}
	for _, name := range []string{"start", "ls", "status", "send", "commit", "push", "sync"} {
		want[name] = true
	}
	got := map[string]bool{}
	for _, cmd := range newRootCmd().Commands() {
		if !cmd.Hidden && cmd.Name() != "help" {
			got[cmd.Name()] = true
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("root help missing approved command %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("root help exposes unapproved command %q", name)
		}
	}
}

func TestHelpTraversalMatchesFlagHelpEverywhere(t *testing.T) {
	t.Parallel()

	var paths [][]string
	WalkCommandTree(newRootCmd(), func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Annotations[AnnotationNodeKind] == NodeInternal {
			return
		}
		paths = append(paths, strings.Fields(strings.TrimPrefix(cmd.CommandPath(), "warden ")))
	})
	for _, path := range paths {
		name := strings.Join(path, " ")
		traversal, err := executeHelp(t, append([]string{"help"}, path...)...)
		if err != nil {
			t.Errorf("help %s: %v", name, err)
			continue
		}
		flag, err := executeHelp(t, append(append([]string{}, path...), "--help")...)
		if err != nil {
			t.Errorf("%s --help: %v", name, err)
			continue
		}
		if traversal != flag {
			t.Errorf("`help %s` differs from `%s --help`", name, name)
		}
	}
}

func TestCanonicalNamespaceAndDeepLeafGoldens(t *testing.T) {
	cases := map[string][]string{
		"backend":             {"help", "backend"},
		"usage":               {"help", "usage"},
		"inspect":             {"help", "inspect"},
		"config":              {"help", "config"},
		"completion":          {"help", "completion"},
		"agent_start":         {"help", "agent", "start"},
		"approval_auto_allow": {"help", "approval", "auto", "allow"},
		"all":                 {"help", "--all"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := executeHelp(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			requireGolden(t, name, got)
		})
	}
}

func TestCompletionGeneratesForEveryShell(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		shell    string
		generate func(*cobra.Command, *bytes.Buffer) error
		want     string
	}{
		{"bash", func(c *cobra.Command, b *bytes.Buffer) error { return c.GenBashCompletionV2(b, true) }, "__start_warden"},
		{"zsh", func(c *cobra.Command, b *bytes.Buffer) error { return c.GenZshCompletion(b) }, "#compdef warden"},
		{"fish", func(c *cobra.Command, b *bytes.Buffer) error { return c.GenFishCompletion(b, true) }, "__warden_perform_completion"},
		{"powershell", func(c *cobra.Command, b *bytes.Buffer) error { return c.GenPowerShellCompletionWithDesc(b) }, "Register-ArgumentCompleter"},
	} {
		t.Run(tc.shell, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.generate(newRootCmd(), &buf); err != nil {
				t.Fatalf("generate %s completion: %v", tc.shell, err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("%s completion script missing %q", tc.shell, tc.want)
			}
		})
	}
}

func complete(t *testing.T, args ...string) []string {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{cobra.ShellCompNoDescRequestCmd}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("completion request %v: %v", args, err)
	}
	var candidates []string
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		candidates = append(candidates, strings.SplitN(line, "\t", 2)[0])
	}
	return candidates
}

func TestCompletionOffersCanonicalPathsAndHidesLegacyAliases(t *testing.T) {
	t.Parallel()

	root := complete(t, "")
	for _, want := range append(append([]string{}, canonicalNamespaces...), "ls", "start", "status", "send", "commit", "push", "sync") {
		if !contains(root, want) {
			t.Errorf("root completion missing %q", want)
		}
	}
	for _, hidden := range []string{"backends", "models", "ctx", "msg", "approvals", "worktree", "stats", "cost", "mcp", "token"} {
		if contains(root, hidden) {
			t.Errorf("root completion advertises hidden alias %q", hidden)
		}
	}
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"agent", []string{"start", "list", "stop", "role", "permission-mode", "compact"}},
		{"git", []string{"commit", "push", "sync", "review"}},
		{"autopilot", []string{"enable", "disable", "run", "land"}},
		{"approval", []string{"list", "answer", "auto"}},
		{"schedule", []string{"create", "list", "show"}},
	} {
		got := complete(t, tc.path, "")
		for _, want := range tc.want {
			if !contains(got, want) {
				t.Errorf("`%s` completion missing %q (got %v)", tc.path, want, got)
			}
		}
	}
}

func TestGeneratedReferenceAgreesWithTheAnnotatedTree(t *testing.T) {
	t.Parallel()

	doc, err := GenerateReference()
	if err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	WalkCommandTree(root, func(cmd *cobra.Command) {
		heading := "\n## " + cmd.CommandPath() + "\n"
		documented := strings.Contains(doc, heading)
		shouldDocument := !cmd.Hidden && cmd.Annotations[AnnotationIncludeInDocs] != "false" && !cmd.IsAdditionalHelpTopicCommand()
		if shouldDocument && !documented {
			t.Errorf("generated reference missing section for %q", cmd.CommandPath())
		}
		if !shouldDocument && documented {
			t.Errorf("generated reference documents hidden command %q", cmd.CommandPath())
		}
	})
	if !strings.Contains(doc, "## Compatibility aliases") {
		t.Fatal("generated reference has no compatibility-alias appendix")
	}
	appendix := doc[strings.Index(doc, "## Compatibility aliases"):]
	for _, alias := range CollectCompatibilityAliases(root) {
		if !strings.Contains(appendix, "| `"+alias.Path+"` |") {
			t.Errorf("alias appendix missing %q", alias.Path)
		}
	}
}

func TestHelpRenderingContactsNothing(t *testing.T) {
	t.Setenv("WARDEN_ADDR", "127.0.0.1:1")
	for _, args := range [][]string{{"help"}, {"help", "--all"}, {"help", "agent"}, {"agent", "start", "--help"}} {
		if _, err := executeHelp(t, args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
