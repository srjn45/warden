package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOperationsCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	pairs := map[string]string{
		"daemon mcp":            "mcp",
		"daemon token":          "token",
		"daemon token generate": "token generate",
		"daemon token show":     "token show",
		"daemon token rotate":   "token rotate",
	}
	for canonical, legacy := range pairs {
		canonicalCmd := findExactCommand(t, root, canonical)
		legacyCmd := findExactCommand(t, root, legacy)
		if !legacyCmd.Hidden {
			t.Errorf("legacy %q should be hidden", legacy)
		}
		if got := legacyCmd.Annotations[AnnotationAliasKind]; got != AliasCompatibility {
			t.Errorf("legacy %q alias kind=%q, want %q", legacy, got, AliasCompatibility)
		}
		wantCanonical := "warden " + canonical
		if got := legacyCmd.Annotations[AnnotationCanonicalPath]; got != wantCanonical {
			t.Errorf("legacy %q canonical=%q, want %q", legacy, got, wantCanonical)
		}
		if got, want := commandFlagSignature(canonicalCmd), commandFlagSignature(legacyCmd); !reflect.DeepEqual(got, want) {
			t.Errorf("%s flags differ from %s: %v != %v", canonical, legacy, got, want)
		}
	}
}

func TestOperationsEntryPointsAreVisible(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"setup", "tutorial", "doctor", "tui", "version"} {
		cmd := findExactCommand(t, root, name)
		if cmd.Hidden {
			t.Errorf("%q should remain a visible entry point", name)
		}
		if got := cmd.Annotations[AnnotationNodeKind]; got != NodeEntryPoint {
			t.Errorf("%q node kind=%q, want %q", name, got, NodeEntryPoint)
		}
		if got := cmd.Annotations[AnnotationHelpGroup]; got != "entry" {
			t.Errorf("%q help group=%q, want entry", name, got)
		}
	}
}

func TestOperationsNamespacesHaveMetadata(t *testing.T) {
	root := newRootCmd()
	cases := map[string]struct {
		group    string
		nodeKind string
	}{
		"schedule":   {"run", NodeNamespace},
		"config":     {"observe", NodeNamespace},
		"daemon":     {"operate", NodeNamespace},
		"completion": {"operate", NodeNamespace},
	}
	for path, want := range cases {
		cmd := findExactCommand(t, root, path)
		if got := cmd.Annotations[AnnotationHelpGroup]; got != want.group {
			t.Errorf("%q group=%q, want %q", path, got, want.group)
		}
		if got := cmd.Annotations[AnnotationNodeKind]; got != want.nodeKind {
			t.Errorf("%q node kind=%q, want %q", path, got, want.nodeKind)
		}
	}
}

func TestDaemonTokenGenerateCanonicalAndAliasMatch(t *testing.T) {
	root := newRootCmd()
	canonical := findExactCommand(t, root, "daemon token generate")
	legacy := findExactCommand(t, root, "token generate")
	if got, want := commandFlagSignature(canonical), commandFlagSignature(legacy); !reflect.DeepEqual(got, want) {
		t.Fatalf("token generate flag signatures differ: %v != %v", got, want)
	}
	var outCanonical, outLegacy strings.Builder
	canonical.SetOut(&outCanonical)
	legacy.SetOut(&outLegacy)
	require.NoError(t, canonical.RunE(canonical, nil))
	require.NoError(t, legacy.RunE(legacy, nil))
	for name, out := range map[string]string{"canonical": outCanonical.String(), "legacy": outLegacy.String()} {
		tok := strings.TrimSpace(out)
		require.Len(t, tok, 64, "%s generate must print a 32-byte hex token", name)
	}
}

func TestOperationsFactoriesAreFresh(t *testing.T) {
	a, b := newDaemonCmd(), newDaemonCmd()
	if a == b || a.Commands()[0] == b.Commands()[0] {
		t.Fatal("daemon factory reused Cobra command pointers")
	}
	mcpA := findExactCommand(t, wrapRoot(a), "daemon mcp")
	mcpB := findExactCommand(t, wrapRoot(b), "daemon mcp")
	if mcpA == mcpB {
		t.Fatal("daemon mcp factory reused command pointer")
	}
}

func TestOperationsProgressiveHelp(t *testing.T) {
	daemonHelp, err := executeHelp(t, "help", "daemon")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(daemonHelp, "Run the warden hub") || !strings.Contains(daemonHelp, "mcp") || !strings.Contains(daemonHelp, "token") {
		t.Fatalf("daemon namespace help missing expected sections: %s", daemonHelp)
	}
	scheduleHelp, err := executeHelp(t, "help", "schedule")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scheduleHelp, "timer-driven triggers") || !strings.Contains(scheduleHelp, "create") {
		t.Fatalf("schedule namespace help missing expected sections: %s", scheduleHelp)
	}
	for name, got := range map[string]string{"daemon": daemonHelp, "schedule": scheduleHelp} {
		want, err := os.ReadFile(filepath.Join("testdata", "help_"+name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Fatalf("%s golden mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
		}
	}
}

func TestScheduleCanonicalPathUnchanged(t *testing.T) {
	root := newRootCmd()
	for _, path := range []string{"schedule", "schedule create", "schedule list", "schedule get"} {
		cmd := findExactCommand(t, root, path)
		if cmd.Hidden {
			t.Errorf("%q should remain visible", path)
		}
		if strings.Contains(cmd.Annotations[AnnotationAliasKind], AliasCompatibility) {
			t.Errorf("%q should not be a compatibility alias", path)
		}
	}
}
