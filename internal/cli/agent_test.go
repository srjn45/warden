package cli

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestAgentNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	pairs := map[string]string{
		"agent list": "ls", "agent start": "start", "agent status": "status",
		"agent digest": "digest", "agent fork": "fork", "agent restore": "restore",
		"agent recover": "recover", "agent adopt": "adopt", "agent attach": "attach",
		"agent stop": "stop", "agent terminate": "terminate", "agent done": "done",
		"agent delete": "delete", "agent remove-worktree": "remove-worktree",
		"agent send": "send", "agent tail": "tail", "agent handoff": "handoff",
		"agent rotate": "rotate", "agent switch": "switch",
		"agent permission-mode set": "set-permission-mode", "agent role set": "set-role",
		"agent role": "role", "agent compact set": "force-compact",
	}
	permanent := map[string]bool{"ls": true, "start": true, "status": true, "send": true}
	for canonical, legacy := range pairs {
		canonicalCmd := findExactCommand(t, root, canonical)
		legacyCmd := findExactCommand(t, root, legacy)
		if legacyCmd.Hidden == permanent[legacy] {
			t.Errorf("legacy %q hidden=%v, permanent=%v", legacy, legacyCmd.Hidden, permanent[legacy])
		}
		wantKind := AliasCompatibility
		if permanent[legacy] {
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
}

func findExactCommand(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	cmd, rest, err := root.Find(strings.Fields(path))
	if err != nil || len(rest) != 0 || strings.TrimPrefix(cmd.CommandPath(), "warden ") != path {
		t.Fatalf("find %q: cmd=%q rest=%v err=%v", path, cmd.CommandPath(), rest, err)
	}
	return cmd
}

func commandFlagSignature(cmd *cobra.Command) map[string]string {
	result := map[string]string{}
	cmd.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
		result[flag.Name] = flag.DefValue + "|" + flag.Usage
	})
	return result
}

func TestAgentCanonicalAliasDispatchAndJSON(t *testing.T) {
	for _, args := range [][]string{{"agent", "status", "A-1", "--json"}, {"status", "A-1", "--json"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			method := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, map[string]string{
				"GET /api/v1/sessions/A-1": `{"id":"A-1","status":"working"}`,
			}, method, nil))
			out, err := runCLI(t, addr, args...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/sessions/A-1"] != http.MethodGet || !strings.Contains(out, `"id": "A-1"`) {
				t.Fatalf("dispatch/output mismatch: method=%v out=%q", method, out)
			}
		})
	}
}

func TestAgentCanonicalAndAliasPreserveDestructiveDecline(t *testing.T) {
	for _, args := range [][]string{{"agent", "remove-worktree", "A-1"}, {"remove-worktree", "A-1"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			called := false
			addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) { called = true })
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetIn(strings.NewReader("n\n"))
			root.SetArgs(append(args, "--addr", addr, "--config", t.TempDir()+"/none.yaml"))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if called || !strings.Contains(out.String(), "aborted") {
				t.Fatalf("decline safeguard changed: called=%v out=%q", called, out.String())
			}
		})
	}
}

func TestAgentProgressiveHelp(t *testing.T) {
	namespace, err := executeHelp(t, "help", "agent")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := executeHelp(t, "help", "agent", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(namespace, "Lifecycle commands deliberately remain distinct") || !strings.Contains(namespace, "remove-worktree") {
		t.Fatalf("agent namespace help lacks lifecycle guidance: %s", namespace)
	}
	if !strings.Contains(leaf, "--keep-record") || strings.Contains(leaf, "agent start") {
		t.Fatalf("agent stop help is not focused: %s", leaf)
	}
	for name, got := range map[string]string{"agent": namespace, "agent_stop": leaf} {
		want, err := os.ReadFile(filepath.Join("testdata", "help_"+name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Fatalf("%s golden mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
		}
	}
}

func TestAgentFactoriesAreFreshAndErrorsMatch(t *testing.T) {
	a, b := newAgentCmd(), newAgentCmd()
	if a == b || a.Commands()[0] == b.Commands()[0] {
		t.Fatal("agent factory reused Cobra command pointers")
	}
	startA := findExactCommand(t, wrapRoot(a), "agent start")
	startB := findExactCommand(t, wrapRoot(b), "agent start")
	if err := startA.Flags().Set("name", "changed"); err != nil {
		t.Fatal(err)
	}
	if got, _ := startB.Flags().GetString("name"); got != "" {
		t.Fatalf("agent factories share flag storage: %q", got)
	}
	_, canonicalErr := runCLI(t, "", "agent", "role", "set", "A-1", "not-a-role")
	_, aliasErr := runCLI(t, "", "set-role", "A-1", "not-a-role")
	if canonicalErr == nil || aliasErr == nil || canonicalErr.Error() != aliasErr.Error() {
		t.Fatalf("canonical/alias errors differ: %v != %v", canonicalErr, aliasErr)
	}
}

func wrapRoot(child *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "warden"}
	root.AddCommand(child)
	return root
}
