package cli

import (
	"net/http"
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

func TestAutopilotNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	pairs := map[string]string{
		"autopilot status": "autopilot list",
		"plan pause":       "autopilot pause",
		"plan resume":      "autopilot resume",
		"plan stop":        "autopilot stop",
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
		if strings.HasPrefix(canonical, "autopilot ") {
			if got, want := commandFlagSignature(canonicalCmd), commandFlagSignature(legacyCmd); !reflect.DeepEqual(got, want) {
				t.Errorf("%s flags differ from %s", canonical, legacy)
			}
		}
	}
	for _, legacy := range []string{"autopilot register", "autopilot start", "autopilot unregister"} {
		legacyCmd := findExactCommand(t, root, legacy)
		if !legacyCmd.Hidden {
			t.Errorf("legacy %q should be hidden", legacy)
		}
		if got := legacyCmd.Annotations[AnnotationAliasKind]; got != AliasCompatibility {
			t.Errorf("legacy %q alias kind=%q, want %q", legacy, got, AliasCompatibility)
		}
		canon := legacyCmd.Annotations[AnnotationCanonicalPath]
		if canon != "warden plan run" && canon != "warden plan stop" {
			t.Errorf("legacy %q canonical=%q, want plan run or plan stop", legacy, canon)
		}
	}
}

func TestAutopilotCanonicalAliasDispatchEquivalence(t *testing.T) {
	methods := map[string]string{}
	bodies := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/autopilot":                  `{"runs":[]}`,
		"POST /api/v1/autopilot/runs/ap-123/stop": `{"run_id":"ap-123","name":"demo","state":"stopped"}`,
		"GET /api/v1/pipelines":                   `{"pipelines":[]}`,
	}, methods, bodies))

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2][]string{
		{{"autopilot", "disable", "--repo", repo}, {"autopilot", "off", "--repo", repo}},
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

func TestAutopilotEnablementAndRunLifecycleCanonicalPaths(t *testing.T) {
	methods := map[string]string{}
	bodies := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/autopilot":                  `{"runs":[]}`,
		"POST /api/v1/autopilot/runs/ap-123/stop": `{"run_id":"ap-123","name":"demo","state":"stopped"}`,
	}, methods, bodies))

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCLI(t, addr, "autopilot", "disable", "--repo", repo)
	if err != nil {
		t.Fatalf("autopilot disable: %v", err)
	}
	if methods["/api/v1/autopilot"] != http.MethodPost ||
		!strings.Contains(bodies["/api/v1/autopilot"], `"enabled":false`) {
		t.Fatalf("repo disable dispatch changed: method=%q body=%q", methods["/api/v1/autopilot"], bodies["/api/v1/autopilot"])
	}
	if !strings.Contains(out, "deprecated") {
		t.Fatalf("repo disable output changed: %q", out)
	}

	// Deprecated flat alias still hits the retired control endpoint.
	out, err = runCLI(t, addr, "autopilot", "stop", "ap-123")
	if err != nil {
		t.Fatalf("autopilot stop alias: %v", err)
	}
	if methods["/api/v1/autopilot/runs/ap-123/stop"] != http.MethodPost {
		t.Fatalf("deprecated stop alias dispatch changed: %q", methods["/api/v1/autopilot/runs/ap-123/stop"])
	}
	if strings.TrimSpace(out) != "ap-123\tdemo\tstopped" {
		t.Fatalf("stop alias output changed: %q", out)
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
