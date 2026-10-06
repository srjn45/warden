package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPipelineNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	pairs := map[string]string{
		"pipeline template list": "pipeline list-templates",
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
		if got, want := legacyCmd.Annotations[AnnotationCanonicalPath], "warden "+canonical; got != want {
			t.Errorf("legacy %q canonical=%q, want %q", legacy, got, want)
		}
		if got, want := commandFlagSignature(canonicalCmd), commandFlagSignature(legacyCmd); !reflect.DeepEqual(got, want) {
			t.Errorf("%s flags differ from %s", canonical, legacy)
		}
	}
}

func TestAutopilotCanonicalAliasDispatchEquivalence(t *testing.T) {
	methods := map[string]string{}
	bodies := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines": `{"pipelines":[]}`,
	}, methods, bodies))

	for _, pair := range [][2][]string{
		{{"pipeline", "template", "list"}, {"pipeline", "list-templates"}},
	} {
		t.Run(strings.Join(pair[0], "_"), func(t *testing.T) {
			methods = map[string]string{}
			bodies = map[string]string{}
			outCanon, errCanon := runCLI(t, addr, pair[0]...)
			outAlias, errAlias := runCLI(t, addr, pair[1]...)
			if (errCanon == nil) != (errAlias == nil) {
				t.Fatalf("error mismatch: canonical=%v alias=%v", errCanon, errAlias)
			}
			if errCanon != nil && errAlias != nil && errCanon.Error() != errAlias.Error() {
				t.Fatalf("error text mismatch: %v != %v", errCanon, errAlias)
			}
			if outCanon != outAlias {
				t.Fatalf("output mismatch:\ncanonical=%q\nalias=%q", outCanon, outAlias)
			}
		})
	}
}

func TestPipelineAutopilotProgressiveHelp(t *testing.T) {
	for name, args := range map[string][]string{
		"pipeline":      {"help", "pipeline"},
		"pipeline_leaf": {"help", "pipeline", "edit-job"},
		"autopilot":     {"help", "autopilot"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := executeHelp(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "help_"+name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("%s golden mismatch\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
			}
		})
	}
}

func TestWritePipelineAutopilotHelpGoldens(t *testing.T) {
	if os.Getenv("WRITE_HELP_GOLDENS") != "1" {
		t.Skip("set WRITE_HELP_GOLDENS=1 to regenerate")
	}
	for name, args := range map[string][]string{
		"pipeline":      {"help", "pipeline"},
		"pipeline_leaf": {"help", "pipeline", "edit-job"},
		"autopilot":     {"help", "autopilot"},
		"namespace":     {"help", "pipeline"},
	} {
		got, err := executeHelp(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "help_"+name+".golden")
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPipelineAutopilotFactoriesAreFresh(t *testing.T) {
	a, b := newPipelineCmd(), newPipelineCmd()
	if a == b || a.Commands()[0] == b.Commands()[0] {
		t.Fatal("pipeline factory reused Cobra command pointers")
	}
	a, b = newAutopilotCmd(), newAutopilotCmd()
	if a == b || a.Commands()[0] == b.Commands()[0] {
		t.Fatal("autopilot factory reused Cobra command pointers")
	}
}
