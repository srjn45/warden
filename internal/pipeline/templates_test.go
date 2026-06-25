package pipeline

import (
	"strings"
	"testing"
)

func TestListTemplatesCoversAllStarters(t *testing.T) {
	got := ListTemplates()
	want := map[string]bool{
		"analyze-implement-review": false,
		"parallel-tasks":           false,
		"test-fix-verify":          false,
		"research-synthesis":       false,
	}
	for _, ti := range got {
		if _, ok := want[ti.Name]; ok {
			want[ti.Name] = true
		}
		if ti.Description == "" {
			t.Errorf("template %q has no description", ti.Name)
		}
		if len(ti.Placeholders) == 0 {
			t.Errorf("template %q reports no placeholders", ti.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("template %q missing from ListTemplates", name)
		}
	}
}

// Every shipped template must render into a valid pipeline once its placeholders
// are filled — this is the guard against a malformed starter.
func TestEveryTemplateRendersToValidSpec(t *testing.T) {
	for _, ti := range ListTemplates() {
		vars := map[string]string{}
		for _, p := range ti.Placeholders {
			switch p {
			case "NAME":
				vars[p] = "demo"
			case "REPO":
				vars[p] = "/tmp/repo"
			default:
				vars[p] = "do the thing"
			}
		}
		rendered, err := RenderTemplate(ti.Name, vars)
		if err != nil {
			t.Fatalf("render %q: %v", ti.Name, err)
		}
		p, err := ParseSpec([]byte(rendered))
		if err != nil {
			t.Fatalf("template %q rendered to invalid spec: %v", ti.Name, err)
		}
		if p.ID != "demo" || p.Repo != "/tmp/repo" {
			t.Errorf("template %q: substitution did not apply (id=%q repo=%q)", ti.Name, p.ID, p.Repo)
		}
		if len(p.Jobs) == 0 {
			t.Errorf("template %q rendered to a pipeline with no jobs", ti.Name)
		}
	}
}

func TestRenderTemplateMissingVar(t *testing.T) {
	_, err := RenderTemplate("analyze-implement-review", map[string]string{
		"NAME": "demo", "REPO": "/r",
	})
	if err == nil {
		t.Fatal("expected an error for the unfilled TASK placeholder")
	}
	if !strings.Contains(err.Error(), "TASK") {
		t.Errorf("error should name the missing placeholder, got: %v", err)
	}
}

func TestRenderTemplateUnknown(t *testing.T) {
	if _, err := RenderTemplate("nope", map[string]string{}); err == nil {
		t.Fatal("expected an error for an unknown template")
	}
	if _, err := RenderTemplate("../secret", map[string]string{}); err == nil {
		t.Fatal("expected an error for a traversal name")
	}
}
