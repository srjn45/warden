package pipeline

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed templates/*.yaml
var templatesFS embed.FS

// placeholderRe matches an upper-snake-case placeholder like {{TASK}} or
// {{TASK_A}}. Whitespace inside the braces is tolerated.
var placeholderRe = regexp.MustCompile(`{{\s*([A-Z][A-Z0-9_]*)\s*}}`)

// TemplateInfo describes one embedded pipeline starter: its name, the one-line
// description from its leading comment, and the placeholder names it expects.
type TemplateInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Placeholders []string `json:"placeholders"`
}

// ListTemplates returns the embedded pipeline templates, sorted by name.
func ListTemplates() []TemplateInfo {
	entries, _ := fs.ReadDir(templatesFS, "templates")
	out := make([]TemplateInfo, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".yaml")
		body, err := templateBody(name)
		if err != nil {
			continue
		}
		out = append(out, TemplateInfo{
			Name:         name,
			Description:  templateDescription(body),
			Placeholders: extractPlaceholders(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RenderTemplate substitutes the {{KEY}} placeholders in the named template
// with vars and returns the rendered YAML spec. It errors if the template is
// unknown or if any placeholder is left without a value.
func RenderTemplate(name string, vars map[string]string) (string, error) {
	body, err := templateBody(name)
	if err != nil {
		return "", err
	}
	var missing []string
	rendered := placeholderRe.ReplaceAllStringFunc(body, func(m string) string {
		key := placeholderRe.FindStringSubmatch(m)[1]
		if v, ok := vars[key]; ok {
			return v
		}
		missing = append(missing, key)
		return m
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("template %q needs values for: %s (pass with --set KEY=VALUE)",
			name, strings.Join(dedupe(missing), ", "))
	}
	return rendered, nil
}

// templateBody returns the raw YAML of the named template, rejecting names that
// would escape the embedded templates/ directory.
func templateBody(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return "", fmt.Errorf("unknown template %q (run `warden pipeline list-templates`)", name)
	}
	data, err := templatesFS.ReadFile(filepath.ToSlash("templates/" + name + ".yaml"))
	if err != nil {
		return "", fmt.Errorf("unknown template %q (run `warden pipeline list-templates`)", name)
	}
	return string(data), nil
}

// templateDescription returns the text of the template's leading `# ...` comment,
// or "" if the file does not start with one.
func templateDescription(body string) string {
	line := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		line = body[:i]
	}
	if !strings.HasPrefix(line, "#") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "#"))
}

// extractPlaceholders returns the distinct placeholder names in body, in first
// appearance order.
func extractPlaceholders(body string) []string {
	var names []string
	for _, m := range placeholderRe.FindAllStringSubmatch(body, -1) {
		names = append(names, m[1])
	}
	return dedupe(names)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
