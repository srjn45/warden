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
		"autopilot enable":         "autopilot on",
		"autopilot disable":        "autopilot off",
		"autopilot run list":       "autopilot list",
		"autopilot run start":      "autopilot start",
		"autopilot run pause":      "autopilot pause",
		"autopilot run resume":     "autopilot resume",
		"autopilot run stop":       "autopilot stop",
		"autopilot run unregister": "autopilot unregister",
		"autopilot land":           "land",
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
		"POST /api/v1/autopilot":                  `{"enabled":false,"enabled_repos":[],"runs":[]}`,
		"POST /api/v1/autopilot/runs/ap-123/stop": `{"run_id":"ap-123","name":"demo","state":"stopped"}`,
		"GET /api/v1/pipelines":                   `{"pipelines":[]}`,
	}, methods, bodies))

	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2][]string{
		{{"autopilot", "disable", "--repo", repo}, {"autopilot", "off", "--repo", repo}},
		{{"autopilot", "run", "stop", "ap-123"}, {"autopilot", "stop", "ap-123"}},
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
		"POST /api/v1/autopilot":                  `{"enabled":false,"enabled_repos":[],"runs":[]}`,
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
	if !strings.Contains(out, "autopilot disabled for ") {
		t.Fatalf("repo disable output changed: %q", out)
	}

	out, err = runCLI(t, addr, "autopilot", "run", "stop", "ap-123")
	if err != nil {
		t.Fatalf("autopilot run stop: %v", err)
	}
	if methods["/api/v1/autopilot/runs/ap-123/stop"] != http.MethodPost {
		t.Fatalf("run stop dispatch changed: %q", methods["/api/v1/autopilot/runs/ap-123/stop"])
	}
	if strings.TrimSpace(out) != "ap-123\tdemo\tstopped" {
		t.Fatalf("run stop output changed: %q", out)
	}
}

func TestPipelineAutopilotProgressiveHelp(t *testing.T) {
	for name, args := range map[string][]string{
		"pipeline":      {"help", "pipeline"},
		"pipeline_leaf": {"help", "pipeline", "edit-job"},
		"autopilot":     {"help", "autopilot"},
		"autopilot_run": {"help", "autopilot", "run"},
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
		"autopilot_run": {"help", "autopilot", "run"},
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
