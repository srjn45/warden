package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPipelineNamespaceCanonicalAndCompatibilityPaths(t *testing.T) {
	root := newRootCmd()
	list := findExactCommand(t, root, "pipeline template list")
	show := findExactCommand(t, root, "pipeline template show")
	if list.Hidden || show.Hidden {
		t.Fatal("template list/show must be visible")
	}
	cmd, rest, err := root.Find([]string{"pipeline", "list-templates"})
	if err != nil || cmd.CommandPath() != "warden pipeline" || len(rest) != 1 || rest[0] != "list-templates" {
		t.Fatalf("list-templates must not resolve as a command: path=%q rest=%v err=%v", cmd.CommandPath(), rest, err)
	}
}

func TestPipelineAutopilotProgressiveHelp(t *testing.T) {
	for name, args := range map[string][]string{
		"pipeline":      {"help", "pipeline"},
		"pipeline_leaf": {"help", "pipeline", "job", "edit"},
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
		"pipeline_leaf": {"help", "pipeline", "job", "edit"},
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
