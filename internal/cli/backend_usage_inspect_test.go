package cli

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/savings"
)

func TestBackendUsageInspectCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	pairs := map[string]string{
		"backend list": "backends list", "backend rescan": "backends rescan",
		"backend tier": "backends tier", "backend default": "backends default",
		"backend enable": "backends enable", "backend disable": "backends disable",
		"backend thinking-mode": "backends thinking-mode",
		"backend model":         "models", "backend model list": "models list", "backend model tier": "models tier",
		"backend suggest": "llm suggest", "backend repl": "repl",
		"usage spend": "spend", "usage savings": "savings", "usage insights": "insights",
		"inspect resources": "stats", "inspect search": "search", "inspect history": "history",
		"inspect audit": "audit log", "inspect export": "export", "inspect import": "import",
		"inspect repair": "repair", "inspect repair sessions": "repair sessions",
	}
	for canonical, legacy := range pairs {
		canonicalCmd := findExactCommand(t, root, canonical)
		legacyCmd := findExactCommand(t, root, legacy)
		if !legacyCmd.Hidden {
			t.Errorf("legacy %q hidden=%v, want true", legacy, legacyCmd.Hidden)
		}
		if got := legacyCmd.Annotations[AnnotationAliasKind]; got != AliasCompatibility {
			t.Errorf("legacy %q alias kind=%q, want %q", legacy, got, AliasCompatibility)
		}
		if got, want := legacyCmd.Annotations[AnnotationCanonicalPath], "warden "+canonical; got != want {
			t.Errorf("legacy %q canonical=%q, want %q", legacy, got, want)
		}
		if got, want := commandFlagSignature(canonicalCmd), commandFlagSignature(legacyCmd); !reflect.DeepEqual(got, want) {
			t.Errorf("%s flags differ from %s: %v != %v", canonical, legacy, got, want)
		}
	}
}

func TestUsageBareKeepsProviderQuotaDispatch(t *testing.T) {
	for _, args := range [][]string{{"usage", "--json"}, {"usage", "--json", "--refresh"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			method := map[string]string{}
			addr := stubDaemon(t, routedDaemon(t, map[string]string{
				"GET /api/v1/usage": `{"schema_version":1,"backends":[]}`,
			}, method, nil))
			out, err := runCLI(t, addr, append(args, "--addr", addr)...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/usage"] != http.MethodGet || !strings.Contains(out, `"backends"`) {
				t.Fatalf("usage dispatch mismatch: method=%v out=%q", method, out)
			}
		})
	}
}

func TestCostDoesNotCaptureUsageBareDispatch(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/spend":   `{"total_usd":1}`,
		"GET /api/v1/savings": `{"events":0}`,
	}, method, nil))
	out, err := runCLI(t, addr, "cost")
	if err != nil {
		t.Fatal(err)
	}
	if method["/api/v1/usage"] != "" || !strings.Contains(out, "SPEND") {
		t.Fatalf("cost should not call usage: method=%v out=%q", method, out)
	}
}

func TestInspectResourcesMatchesStatsJSON(t *testing.T) {
	payload := `{"system":{"total_bytes":1},"agents":[],"daemon":{}}`
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/metrics": payload,
	}, nil, nil))
	canonical, err := runCLI(t, addr, "inspect", "resources", "--json")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := runCLI(t, addr, "stats", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != legacy {
		t.Fatalf("inspect resources and stats diverged:\ncanonical: %s\nlegacy: %s", canonical, legacy)
	}
}

func TestBackendCanonicalAliasDispatch(t *testing.T) {
	method := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/backends": backendsStateJSON,
	}, method, nil))
	for _, args := range [][]string{{"backend", "list"}, {"backends", "list"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			out, err := runCLI(t, addr, append(args, "--addr", addr)...)
			if err != nil {
				t.Fatal(err)
			}
			if method["/api/v1/backends"] != http.MethodGet || !strings.Contains(out, "claude") {
				t.Fatalf("dispatch/output mismatch: method=%v out=%q", method, out)
			}
		})
	}
}

func TestBackendUsageInspectFactoriesAreFresh(t *testing.T) {
	for name, factory := range map[string]func() *cobra.Command{
		"backend": newBackendCmd, "usage": newUsageNamespaceCmd, "inspect": newInspectCmd,
	} {
		t.Run(name, func(t *testing.T) {
			a, b := factory(), factory()
			if a == b || a.Commands()[0] == b.Commands()[0] {
				t.Fatalf("%s factory reused Cobra command pointers", name)
			}
		})
	}
}

func TestBackendUsageInspectProgressiveHelp(t *testing.T) {
	for _, tc := range []struct {
		path    []string
		golden  string
		contain string
	}{
		{[]string{"backend"}, "help_backend", "agent-backend registry"},
		{[]string{"usage"}, "help_usage", "provider usage"},
		{[]string{"inspect"}, "help_inspect", "Resource samples"},
		{[]string{"inspect", "resources"}, "help_inspect_resources", "--history"},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := executeHelp(t, append([]string{"help"}, tc.path...)...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, tc.contain) {
				t.Fatalf("%s help missing %q: %s", strings.Join(tc.path, "_"), tc.contain, got)
			}
		})
	}
}

func TestUsageSpendCanonicalMatchesLegacyJSON(t *testing.T) {
	addr := costStubDaemon(t, sampleSpendReport(), &savings.Summary{})
	canonical, err := runCLI(t, addr, "usage", "spend", "--json")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := runCLI(t, addr, "spend", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != legacy {
		t.Fatalf("usage spend and spend diverged")
	}
	var a, b map[string]any
	_ = json.Unmarshal([]byte(canonical), &a)
	_ = json.Unmarshal([]byte(legacy), &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("JSON payloads differ")
	}
}

func TestInspectRepairPreservesSafetyFlags(t *testing.T) {
	for _, args := range [][]string{{"inspect", "repair", "sessions"}, {"repair", "sessions"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd := findExactCommand(t, newRootCmd(), strings.Join(args, " "))
			if cmd.Flags().Lookup("apply") == nil || cmd.Flags().Lookup("backup") == nil {
				t.Fatalf("repair safety flags missing on %v", args)
			}
		})
	}
}
